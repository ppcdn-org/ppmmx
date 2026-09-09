package mpegts

import (
	"testing"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
	tscodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/codecs"
	"github.com/stretchr/testify/require"
)

func TestValidateVideoTracks(t *testing.T) {
	track := func(pid uint16, codec tscodecs.Codec) *mpegts.Track {
		return &mpegts.Track{PID: pid, Codec: codec}
	}

	t.Run("single video keeps codec compatibility", func(t *testing.T) {
		tracks, err := ValidateVideoTracks([]*mpegts.Track{track(100, &tscodecs.H265{})})
		require.NoError(t, err)
		require.Len(t, tracks, 1)
	})

	t.Run("multiple H264 tracks preserve parser order and PIDs", func(t *testing.T) {
		first := track(100, &tscodecs.H264{})
		second := track(101, &tscodecs.H264{})
		tracks, err := ValidateVideoTracks([]*mpegts.Track{first, second})
		require.NoError(t, err)
		require.Equal(t, []*mpegts.Track{first, second}, tracks)
	})

	t.Run("multiple video tracks require H264", func(t *testing.T) {
		_, err := ValidateVideoTracks([]*mpegts.Track{
			track(100, &tscodecs.H264{}),
			track(101, &tscodecs.H265{}),
		})
		require.EqualError(t, err, "SRT publish with multiple video tracks must contain H264 only (PID 101 is *codecs.H265)")
	})

	t.Run("maximum layer count", func(t *testing.T) {
		tracks := make([]*mpegts.Track, MaxVideoLayers+1)
		for i := range tracks {
			tracks[i] = track(uint16(100+i), &tscodecs.H264{})
		}

		_, err := ValidateVideoTracks(tracks)
		require.EqualError(t, err, "SRT publish contains 5 video tracks, maximum is 4")
	})
}
