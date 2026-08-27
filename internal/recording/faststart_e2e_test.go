package recording

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	rtspformat "github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/pmp4"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/recorder"
	"github.com/bluenviron/mediamtx/internal/stream"
	"github.com/bluenviron/mediamtx/internal/test"
	"github.com/bluenviron/mediamtx/internal/unit"
)

// TestFaststartRemuxOfRealRecorderOutput drives the actual recorder to
// produce a segment - init section with its mtxi box, several moof/mdat
// parts, the duration patched into mvhd on close - and remuxes that,
// rather than a file this package built itself. This is the case that
// matters in production, and the one that a hand-built fixture can drift
// away from.
func TestFaststartRemuxOfRealRecorderOutput(t *testing.T) {
	desc := &description.Session{Medias: []*description.Media{
		{
			Type: description.MediaTypeVideo,
			Formats: []rtspformat.Format{&rtspformat.H264{
				PayloadTyp:        96,
				PacketizationMode: 1,
			}},
		},
		{
			Type: description.MediaTypeAudio,
			Formats: []rtspformat.Format{&rtspformat.MPEG4Audio{
				PayloadTyp: 96,
				Config: &mpeg4audio.AudioSpecificConfig{
					Type:         2,
					SampleRate:   44100,
					ChannelCount: 2,
				},
				SizeLength:       13,
				IndexLength:      3,
				IndexDeltaLength: 3,
			}},
		},
	}}

	strm := &stream.Stream{
		OrigDesc:          desc,
		WriteQueueSize:    512,
		RTPMaxPayloadSize: 1450,
		Parent:            test.NilLogger,
	}
	require.NoError(t, strm.Initialize())
	defer strm.Close()

	subStream := &stream.SubStream{Stream: strm, UseRTPPackets: false}
	require.NoError(t, subStream.Initialize())

	dir := t.TempDir()

	rec := &recorder.Recorder{
		PathFormat:      filepath.Join(dir, "%path/%Y-%m-%d_%H-%M-%S-%f"),
		Format:          conf.RecordFormatFMP4,
		PartDuration:    100 * time.Millisecond,
		MaxPartSize:     50 * 1024 * 1024,
		SegmentDuration: 1 * time.Hour, // one segment for the whole test
		PathName:        "mypath",
		Stream:          strm,
		Parent:          test.NilLogger,
	}
	rec.Initialize()

	const sampleCount = 10

	for i := range sampleCount {
		subStream.WriteUnit(desc.Medias[0], desc.Medias[0].Formats[0], &unit.Unit{
			PTS: int64(i) * 200 * 90000 / 1000,
			NTP: time.Date(2008, 5, 20, 22, 15, 25, 0, time.UTC).
				Add(time.Duration(i) * 200 * time.Millisecond),
			Payload: unit.PayloadH264{
				test.FormatH264.SPS,
				test.FormatH264.PPS,
				{5}, // IDR
			},
		})

		subStream.WriteUnit(desc.Medias[1], desc.Medias[1].Formats[0], &unit.Unit{
			PTS: int64(i) * 200 * 44100 / 1000,
			NTP: time.Date(2008, 5, 20, 22, 15, 25, 0, time.UTC).
				Add(time.Duration(i) * 200 * time.Millisecond),
			Payload: unit.PayloadMPEG4Audio{{1, 2, 3, 4}},
		})

		time.Sleep(20 * time.Millisecond)
	}

	time.Sleep(100 * time.Millisecond)
	rec.Close()

	input := filepath.Join(dir, "mypath", "2008-05-20_22-15-25-000000.mp4")
	require.FileExists(t, input)

	out, cleanup, err := faststartRemux(input)
	require.NoError(t, err)
	defer cleanup()

	require.Equal(t, []string{"ftyp", "moov", "mdat"}, boxOffsets(t, out))

	fo, err := os.Open(out)
	require.NoError(t, err)
	defer fo.Close()

	var pres pmp4.Presentation
	require.NoError(t, pres.Unmarshal(fo),
		"the remuxed recording must parse back as a plain presentation")
	require.Len(t, pres.Tracks, 2)

	// The recorder holds one sample back per track (it needs the next
	// sample's DTS to know the current one's duration), so the segment
	// carries every sample but the last of each track.
	for _, track := range pres.Tracks {
		require.Equal(t, sampleCount-1, len(track.Samples),
			"track %d", track.ID)
	}

	// Sanity-check the video timing survived: 200ms at 90kHz.
	video := pres.Tracks[0]
	for i, sa := range video.Samples[:len(video.Samples)-1] {
		require.Equal(t, uint32(18000), sa.Duration, "video sample %d", i)
	}
}
