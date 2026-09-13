package forward

import (
	"testing"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/stretchr/testify/require"
)

// A ladder must be counted whatever codec it uses: counting only H264 sent
// a HEVC ladder down setupOutboundTracks' single-track path, which forwarded
// its first layer and dropped the rest - the downstream node then saw one
// video track instead of three.
func TestSimulcastLayerCount(t *testing.T) {
	video := func(f format.Format) *description.Media {
		return &description.Media{Type: description.MediaTypeVideo, Formats: []format.Format{f}}
	}
	audio := &description.Media{
		Type:    description.MediaTypeAudio,
		Formats: []format.Format{&format.Opus{PayloadTyp: 96, ChannelCount: 2}},
	}

	for _, ca := range []struct {
		name   string
		medias []*description.Media
		want   int
	}{
		{"empty", nil, 0},
		{"audio only", []*description.Media{audio}, 0},
		{
			"single H264",
			[]*description.Media{video(&format.H264{}), audio},
			1,
		},
		{
			"single H265",
			[]*description.Media{video(&format.H265{}), audio},
			1,
		},
		{
			"H264 ladder",
			[]*description.Media{video(&format.H264{}), video(&format.H264{}), video(&format.H264{}), audio},
			3,
		},
		{
			"H265 ladder",
			[]*description.Media{video(&format.H265{}), video(&format.H265{}), video(&format.H265{}), audio},
			3,
		},
		{
			"non-Simulcast video codec is not counted",
			[]*description.Media{video(&format.VP8{}), video(&format.VP8{}), audio},
			0,
		},
	} {
		t.Run(ca.name, func(t *testing.T) {
			require.Equal(t, ca.want, simulcastLayerCount(&description.Session{Medias: ca.medias}))
		})
	}
}
