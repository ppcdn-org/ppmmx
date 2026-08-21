package forward

import (
	"testing"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/stretchr/testify/require"
)

func TestSplitLayers(t *testing.T) {
	h264a := &description.Media{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PayloadTyp: 96}}}
	h264b := &description.Media{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PayloadTyp: 96}}}
	h265 := &description.Media{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H265{PayloadTyp: 96}}}
	audio := &description.Media{Type: description.MediaTypeAudio, Formats: []format.Format{&format.Opus{PayloadTyp: 111}}}

	orig := &description.Session{Medias: []*description.Media{h264a, h265, h264b, audio}}

	layers := SplitLayers("mystream", orig)

	require.Len(t, layers, 2, "non-H264 video must be skipped")

	require.Equal(t, "mystream", layers[0].StreamKey)
	require.Equal(t, []*description.Media{h264a, audio}, layers[0].Desc.Medias)

	require.Equal(t, "mystream_standard", layers[1].StreamKey)
	require.Equal(t, []*description.Media{h264b, audio}, layers[1].Desc.Medias)
}

func TestSplitLayersFourLayerNaming(t *testing.T) {
	var videoMedias []*description.Media
	for i := 0; i < 4; i++ {
		videoMedias = append(videoMedias, &description.Media{
			Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PayloadTyp: 96}},
		})
	}
	audio := &description.Media{Type: description.MediaTypeAudio, Formats: []format.Format{&format.Opus{PayloadTyp: 111}}}

	orig := &description.Session{Medias: append(append([]*description.Media{}, videoMedias...), audio)}

	layers := SplitLayers("table1-fwv", orig)

	require.Len(t, layers, 4)
	require.Equal(t, "table1-fwv", layers[0].StreamKey)
	require.Equal(t, "table1-fwv_standard", layers[1].StreamKey)
	require.Equal(t, "table1-fwv_economic", layers[2].StreamKey)
	require.Equal(t, "table1-fwv_lite", layers[3].StreamKey)
}

func TestSplitLayersNoVideo(t *testing.T) {
	audio := &description.Media{Type: description.MediaTypeAudio, Formats: []format.Format{&format.Opus{PayloadTyp: 111}}}
	orig := &description.Session{Medias: []*description.Media{audio}}

	layers := SplitLayers("mystream", orig)

	require.Empty(t, layers)
}
