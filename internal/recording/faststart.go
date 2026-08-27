package recording

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	amp4 "github.com/abema/go-mp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/pmp4"

	// The recorder stores a custom "mtxi" box in the init section's udta
	// (see recordstore.Mtxi). go-mp4 refuses to unmarshal a box type it
	// doesn't know, so reading a recording at all depends on the
	// registration in recordstore's init - without this import, whether
	// that registration happens is left to whatever else the binary
	// happens to link.
	_ "github.com/bluenviron/mediamtx/internal/recordstore"
)

// sampleFlagIsNonSyncSample is the trun sample flag marking a sample that
// is not a random access point (same constant as internal/playback).
const sampleFlagIsNonSyncSample = 1 << 16

// maxPayloadSize bounds what a single-mdat MP4 can address: pmp4 writes a
// 32-bit mdat size and 32-bit chunk offsets, so a recording with more
// sample data than this cannot be expressed as a faststart file at all.
const maxPayloadSize = math.MaxUint32 - 8

// faststartRemux rewrites filePath - a fragmented MP4 as written by the
// recorder, with moov first but sample data split across repeated
// moof/mdat pairs - into a sibling "<name>.faststart.mp4" carrying the
// same samples in a conventional single moov+mdat layout, so the uploaded
// file plays in a plain <video src="..."> tag, or in any simple
// downloader/player, without buffering the whole thing first.
//
// This is what "ffmpeg -c copy -movflags +faststart" produces, done
// in-process: sample payloads are copied verbatim, nothing is re-encoded,
// and the deployment needs no ffmpeg binary next to it. The heavy lifting
// is pmp4's, the same writer the playback server uses for
// "/get?format=mp4".
//
// The caller is expected to treat a failure as non-fatal and upload the
// original file unremuxed rather than dropping the recording.
func faststartRemux(filePath string) (string, func(), error) {
	noop := func() {}

	fi, err := os.Open(filePath)
	if err != nil {
		return "", noop, err
	}
	defer fi.Close()

	tracks, err := readFragmentedMP4(fi)
	if err != nil {
		return "", noop, err
	}

	out := strings.TrimSuffix(filePath, filepath.Ext(filePath)) + ".faststart.mp4"
	cleanup := func() { os.Remove(out) }

	fo, err := os.Create(out)
	if err != nil {
		return "", noop, err
	}

	// samples are pulled from fi (still open) while this writes.
	err = pmp4.Presentation{Tracks: tracks}.Marshal(fo)

	err2 := fo.Close()
	if err == nil {
		err = err2
	}
	if err != nil {
		cleanup()
		return "", noop, err
	}

	return out, cleanup, nil
}

// remuxTrack accumulates one track's samples while the fragmented input is
// scanned. lastDTS/endDTS carry the timing state needed to fill in each
// sample's duration: a sample's duration is the distance to the *next*
// sample's DTS, which is only known one sample later - and, at a fragment
// boundary, comes from the next fragment's tfdt rather than from the
// previous trun, so that a gap between fragments isn't silently dropped.
type remuxTrack struct {
	pmp4.Track
	lastDTS int64
	endDTS  int64
	started bool
}

func findRemuxTrack(tracks []*remuxTrack, id int) *remuxTrack {
	for _, track := range tracks {
		if track.ID == id {
			return track
		}
	}
	return nil
}

// addSample appends one sample, closing the duration of the previous one.
func (t *remuxTrack) addSample(dts int64, sample *pmp4.Sample) {
	if !t.started {
		t.TimeOffset = int32(dts)
		t.started = true
	} else {
		t.Samples[len(t.Samples)-1].Duration = uint32(max(dts-t.lastDTS, 0))
	}

	t.Samples = append(t.Samples, sample)
	t.lastDTS = dts
}

// finalize closes the duration of the last sample, which has no successor
// to be measured against and so keeps the duration declared by its trun.
func (t *remuxTrack) finalize() {
	if len(t.Samples) != 0 {
		t.Samples[len(t.Samples)-1].Duration = uint32(max(t.endDTS-t.lastDTS, 0))
	}
}

// readFragmentedMP4 reads fi's init section and every moof/mdat pair that
// follows it, returning the tracks that carry at least one sample, ready
// to be marshaled as a presentation. Sample payloads are not loaded: each
// sample gets a GetPayload that reads its bytes from fi on demand, so
// remuxing a long recording costs sample tables, not the media itself.
func readFragmentedMP4(fi *os.File) ([]*pmp4.Track, error) {
	var init fmp4.Init
	err := init.Unmarshal(fi)
	if err != nil {
		return nil, fmt.Errorf("read init: %w", err)
	}

	tracks := make([]*remuxTrack, len(init.Tracks))
	for i, initTrack := range init.Tracks {
		tracks[i] = &remuxTrack{
			Track: pmp4.Track{
				ID:        initTrack.ID,
				TimeScale: initTrack.TimeScale,
				Codec:     initTrack.Codec,
			},
		}
	}

	var curTrack *remuxTrack
	var moofOffset uint64
	var payloadSize uint64

	_, err = amp4.ReadBoxStructure(fi, func(h *amp4.ReadHandle) (any, error) {
		switch h.BoxInfo.Type.String() {
		case "moof":
			moofOffset = h.BoxInfo.Offset
			return h.Expand()

		case "traf":
			return h.Expand()

		case "tfhd":
			box, _, err2 := h.ReadPayload()
			if err2 != nil {
				return nil, err2
			}
			tfhd := box.(*amp4.Tfhd)

			curTrack = findRemuxTrack(tracks, int(tfhd.TrackID))
			if curTrack == nil {
				return nil, fmt.Errorf("invalid track ID: %v", tfhd.TrackID)
			}

		case "tfdt":
			box, _, err2 := h.ReadPayload()
			if err2 != nil {
				return nil, err2
			}
			tfdt := box.(*amp4.Tfdt)

			curTrack.endDTS = int64(tfdt.GetBaseMediaDecodeTime())

		case "trun":
			box, _, err2 := h.ReadPayload()
			if err2 != nil {
				return nil, err2
			}
			trun := box.(*amp4.Trun)

			// Per-sample duration/size/flags are read straight off the
			// entry rather than falling back to tfhd's defaults, and
			// composition offsets are read as v1: the recorder's writer
			// always emits them that way, and this mirrors how the
			// playback server reads back the very same files
			// (see internal/playback/segment_fmp4.go).
			offset := moofOffset + uint64(trun.DataOffset)

			for _, e := range trun.Entries {
				payloadSize += uint64(e.SampleSize)
				if payloadSize > maxPayloadSize {
					return nil, fmt.Errorf("recording is too large to be remuxed into a single mdat")
				}

				sampleOffset := int64(offset)
				sampleSize := e.SampleSize

				curTrack.addSample(curTrack.endDTS, &pmp4.Sample{
					PTSOffset:       e.SampleCompositionTimeOffsetV1,
					IsNonSyncSample: (e.SampleFlags & sampleFlagIsNonSyncSample) != 0,
					PayloadSize:     sampleSize,
					GetPayload: func() ([]byte, error) {
						payload := make([]byte, sampleSize)
						_, err3 := fi.ReadAt(payload, sampleOffset)
						if err3 != nil {
							return nil, err3
						}
						return payload, nil
					},
				})

				offset += uint64(e.SampleSize)
				curTrack.endDTS += int64(e.SampleDuration)
			}
		}

		return nil, nil
	})
	if err != nil {
		return nil, err
	}

	var ret []*pmp4.Track
	for _, track := range tracks {
		if len(track.Samples) != 0 {
			track.finalize()
			ret = append(ret, &track.Track)
		}
	}

	if len(ret) == 0 {
		return nil, fmt.Errorf("no samples found")
	}

	return ret, nil
}
