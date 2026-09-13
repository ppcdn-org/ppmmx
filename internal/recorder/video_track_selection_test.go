package recorder

import (
	"testing"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	rtspformat "github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/stretchr/testify/require"
)

func TestPickRecordedVideo(t *testing.T) {
	video := func(f rtspformat.Format) *description.Media {
		return &description.Media{Type: description.MediaTypeVideo, Formats: []rtspformat.Format{f}}
	}
	audio := &description.Media{
		Type:    description.MediaTypeAudio,
		Formats: []rtspformat.Format{&rtspformat.Opus{PayloadTyp: 96, ChannelCount: 2}},
	}

	t.Run("no video", func(t *testing.T) {
		require.Nil(t, pickRecordedVideo([]*description.Media{audio}))
		require.Nil(t, pickRecordedVideo(nil))
	})

	t.Run("single video is taken", func(t *testing.T) {
		v := video(&rtspformat.H264{})
		require.Same(t, v, pickRecordedVideo([]*description.Media{v, audio}))
	})

	t.Run("first layer of a H264 ladder", func(t *testing.T) {
		first := video(&rtspformat.H264{})
		medias := []*description.Media{first, video(&rtspformat.H264{}), video(&rtspformat.H264{}), audio}
		require.Same(t, first, pickRecordedVideo(medias))
	})

	t.Run("first layer of a HEVC-only ladder", func(t *testing.T) {
		// no H264 to prefer: recording still happens, in HEVC
		first := video(&rtspformat.H265{})
		medias := []*description.Media{first, video(&rtspformat.H265{}), video(&rtspformat.H265{}), audio}
		require.Same(t, first, pickRecordedVideo(medias))
	})

	t.Run("H264 wins over another codec whatever the order", func(t *testing.T) {
		h264 := video(&rtspformat.H264{})
		h265 := video(&rtspformat.H265{})

		require.Same(t, h264, pickRecordedVideo([]*description.Media{h264, h265, audio}))
		require.Same(t, h264, pickRecordedVideo([]*description.Media{h265, h264, audio}))
	})

	t.Run("H264 wins over a HEVC ladder", func(t *testing.T) {
		h264 := video(&rtspformat.H264{})
		medias := []*description.Media{
			video(&rtspformat.H265{}), video(&rtspformat.H265{}), h264, audio,
		}
		require.Same(t, h264, pickRecordedVideo(medias))
	})
}
