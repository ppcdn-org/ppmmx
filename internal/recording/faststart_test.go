package recording

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	amp4 "github.com/abema/go-mp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4/seekablebuffer"
	mcodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/pmp4"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/recordstore"
	"github.com/bluenviron/mediamtx/internal/test"
)

// writeFragmentedMP4 builds the same layout the recorder produces - an
// init section followed by one moof/mdat pair per part - using the same
// mediacommon writers, so the remuxer is exercised against real recorder
// output rather than a hand-rolled approximation. That includes the
// recorder's own mtxi box in udta (see recordstore), which the remuxer
// has to read past.
func writeFragmentedMP4(t *testing.T, path string, parts []*fmp4.Part) {
	t.Helper()

	var buf seekablebuffer.Buffer

	init := fmp4.Init{
		Tracks: []*fmp4.InitTrack{
			{
				ID:        1,
				TimeScale: 90000,
				Codec: &mcodecs.H264{
					SPS: test.FormatH264.SPS,
					PPS: test.FormatH264.PPS,
				},
			},
			{
				ID:        2,
				TimeScale: 48000,
				Codec:     &mcodecs.Opus{ChannelCount: 2},
			},
		},
		UserData: []amp4.IBox{
			&recordstore.Mtxi{
				StreamID:      uuid.MustParse("6bf7f2b1-4b1f-4b1f-8b1f-4b1f4b1f4b1f"),
				SegmentNumber: 1,
				DTS:           0,
				NTP:           time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC).UnixNano(),
			},
		},
	}
	require.NoError(t, init.Marshal(&buf))

	for _, part := range parts {
		require.NoError(t, part.Marshal(&buf))
	}

	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o644))
}

func videoSample(payload []byte, duration uint32, isNonSync bool) *fmp4.Sample {
	return &fmp4.Sample{
		Duration:        duration,
		IsNonSyncSample: isNonSync,
		Payload:         payload,
	}
}

// boxOffsets reports where each top-level box starts, in file order.
func boxOffsets(t *testing.T, path string) []string {
	t.Helper()

	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()

	var types []string
	_, err = amp4.ReadBoxStructure(f, func(h *amp4.ReadHandle) (any, error) {
		if len(h.Path) == 1 {
			types = append(types, h.BoxInfo.Type.String())
		}
		return nil, nil
	})
	require.NoError(t, err)

	return types
}

// TestFaststartRemuxProducesNonFragmentedMP4 is the core guarantee: the
// output is a conventional file whose moov precedes a single mdat and
// contains no fragments, which is what lets a plain <video src> start
// playing without downloading the whole recording first.
func TestFaststartRemuxProducesNonFragmentedMP4(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input.mp4")

	writeFragmentedMP4(t, input, []*fmp4.Part{
		{
			SequenceNumber: 1,
			Tracks: []*fmp4.PartTrack{{
				ID:       1,
				BaseTime: 0,
				Samples: []*fmp4.Sample{
					videoSample([]byte{0x01, 0x02, 0x03}, 3000, false),
					videoSample([]byte{0x04, 0x05, 0x06}, 3000, true),
				},
			}},
		},
		{
			SequenceNumber: 2,
			Tracks: []*fmp4.PartTrack{{
				ID:       1,
				BaseTime: 6000,
				Samples: []*fmp4.Sample{
					videoSample([]byte{0x07, 0x08, 0x09}, 3000, true),
				},
			}},
		},
	})

	out, cleanup, err := faststartRemux(input)
	require.NoError(t, err)
	require.NotEmpty(t, out)
	require.FileExists(t, out)
	require.Equal(t, filepath.Join(dir, "input.faststart.mp4"), out)

	require.Equal(t, []string{"ftyp", "moov", "mdat"}, boxOffsets(t, out),
		"a faststart file is ftyp, then moov, then one mdat - nothing else")

	data, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Equal(t, -1, bytes.Index(data, []byte("moof")), "remuxed output must not be fragmented")

	cleanup()
	_, statErr := os.Stat(out)
	require.True(t, os.IsNotExist(statErr), "cleanup should remove the remuxed file")
}

// TestFaststartRemuxPreservesSamples checks the remux is a true stream
// copy: every sample's payload, sync flag and timing survives, across
// both tracks and across the fragment boundary.
func TestFaststartRemuxPreservesSamples(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input.mp4")

	writeFragmentedMP4(t, input, []*fmp4.Part{
		{
			SequenceNumber: 1,
			Tracks: []*fmp4.PartTrack{
				{
					ID:       1,
					BaseTime: 0,
					Samples: []*fmp4.Sample{
						videoSample([]byte{0xaa, 0xaa}, 3000, false),
						videoSample([]byte{0xbb, 0xbb, 0xbb}, 3000, true),
					},
				},
				{
					ID:       2,
					BaseTime: 0,
					Samples: []*fmp4.Sample{
						{Duration: 960, Payload: []byte{0x11}},
						{Duration: 960, Payload: []byte{0x22}},
					},
				},
			},
		},
		{
			SequenceNumber: 2,
			Tracks: []*fmp4.PartTrack{{
				ID:       1,
				BaseTime: 6000,
				Samples: []*fmp4.Sample{
					videoSample([]byte{0xcc}, 3000, false),
				},
			}},
		},
	})

	out, cleanup, err := faststartRemux(input)
	require.NoError(t, err)
	defer cleanup()

	fo, err := os.Open(out)
	require.NoError(t, err)
	defer fo.Close()

	var pres pmp4.Presentation
	require.NoError(t, pres.Unmarshal(fo))
	require.Len(t, pres.Tracks, 2)

	video := pres.Tracks[0]
	require.Equal(t, 1, video.ID)
	require.Equal(t, uint32(90000), video.TimeScale)
	require.Len(t, video.Samples, 3)

	payloads := make([][]byte, len(video.Samples))
	for i, sa := range video.Samples {
		payloads[i], err = sa.GetPayload()
		require.NoError(t, err)
	}
	require.Equal(t, [][]byte{{0xaa, 0xaa}, {0xbb, 0xbb, 0xbb}, {0xcc}}, payloads)

	require.False(t, video.Samples[0].IsNonSyncSample)
	require.True(t, video.Samples[1].IsNonSyncSample)
	require.False(t, video.Samples[2].IsNonSyncSample,
		"sync flag of a sample carried by a later fragment must survive")

	// Durations of all but the last sample are derived from the distance
	// to the next sample's DTS - including across the fragment boundary,
	// where it comes from the next fragment's tfdt.
	require.Equal(t, uint32(3000), video.Samples[0].Duration)
	require.Equal(t, uint32(3000), video.Samples[1].Duration)
	require.Equal(t, uint32(3000), video.Samples[2].Duration)

	audio := pres.Tracks[1]
	require.Equal(t, 2, audio.ID)
	require.Equal(t, uint32(48000), audio.TimeScale)
	require.Len(t, audio.Samples, 2)
}

// TestFaststartRemuxSkipsTracksWithoutSamples mirrors the playback muxer's
// behavior: a track declared in the init section but never written to
// must not reach the output, where an empty track produces a file some
// players reject.
func TestFaststartRemuxSkipsTracksWithoutSamples(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input.mp4")

	writeFragmentedMP4(t, input, []*fmp4.Part{{
		SequenceNumber: 1,
		Tracks: []*fmp4.PartTrack{{
			ID:       1,
			BaseTime: 0,
			Samples: []*fmp4.Sample{
				videoSample([]byte{0x01}, 3000, false),
			},
		}},
	}})

	out, cleanup, err := faststartRemux(input)
	require.NoError(t, err)
	defer cleanup()

	fo, err := os.Open(out)
	require.NoError(t, err)
	defer fo.Close()

	var pres pmp4.Presentation
	require.NoError(t, pres.Unmarshal(fo))
	require.Len(t, pres.Tracks, 1, "the audio track never received a sample")
	require.Equal(t, 1, pres.Tracks[0].ID)
}

// TestFaststartRemuxRejectsInputWithoutSamples covers a recorder file that
// was created but closed before any part was written: there is nothing to
// remux, and the caller must fall back to uploading the original.
func TestFaststartRemuxRejectsInputWithoutSamples(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input.mp4")

	writeFragmentedMP4(t, input, nil)

	out, cleanup, err := faststartRemux(input)
	require.Error(t, err)
	require.Empty(t, out)
	cleanup()

	_, statErr := os.Stat(filepath.Join(dir, "input.faststart.mp4"))
	require.True(t, os.IsNotExist(statErr), "a failed remux must not leave a partial file behind")
}

func TestFaststartRemuxRejectsNonMP4Input(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input.mp4")
	require.NoError(t, os.WriteFile(input, []byte("this is not an MP4 file"), 0o644))

	out, _, err := faststartRemux(input)
	require.Error(t, err)
	require.Empty(t, out)
}
