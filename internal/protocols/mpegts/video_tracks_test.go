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

	t.Run("multiple H265 tracks preserve parser order and PIDs", func(t *testing.T) {
		first := track(100, &tscodecs.H265{})
		second := track(101, &tscodecs.H265{})
		tracks, err := ValidateVideoTracks([]*mpegts.Track{first, second})
		require.NoError(t, err)
		require.Equal(t, []*mpegts.Track{first, second}, tracks)
	})

	t.Run("multiple video tracks require a single codec", func(t *testing.T) {
		_, err := ValidateVideoTracks([]*mpegts.Track{
			track(100, &tscodecs.H264{}),
			track(101, &tscodecs.H265{}),
		})
		require.EqualError(t, err,
			"SRT publish with multiple video tracks must use a single codec "+
				"(PID 101 is *codecs.H265, expected *codecs.H264)")
	})

	t.Run("maximum layer count", func(t *testing.T) {
		tracks := make([]*mpegts.Track, MaxVideoLayers+1)
		for i := range tracks {
			tracks[i] = track(uint16(100+i), &tscodecs.H264{})
		}

		_, err := ValidateVideoTracks(tracks)
		require.EqualError(t, err, "SRT publish contains 5 h264 video tracks, maximum is 4")
	})

	t.Run("HEVC is capped lower than H264", func(t *testing.T) {
		tracks := make([]*mpegts.Track, MaxHEVCVideoLayers+1)
		for i := range tracks {
			tracks[i] = track(uint16(100+i), &tscodecs.H265{})
		}

		_, err := ValidateVideoTracks(tracks)
		require.EqualError(t, err, "SRT publish contains 4 hevc video tracks, maximum is 3")

		// the same count is fine for H264
		for i := range tracks {
			tracks[i] = track(uint16(100+i), &tscodecs.H264{})
		}
		_, err = ValidateVideoTracks(tracks)
		require.NoError(t, err)
	})

	t.Run("VideoCodecName", func(t *testing.T) {
		require.Equal(t, "h264", VideoCodecName(&tscodecs.H264{}))
		require.Equal(t, "hevc", VideoCodecName(&tscodecs.H265{}))
		require.Equal(t, "", VideoCodecName(&tscodecs.MPEG4Video{}))
	})
}
