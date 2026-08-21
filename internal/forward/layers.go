package forward

import (
	"fmt"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
)

// Layer is one video quality layer to forward, paired with the shared audio
// (if any) of the path it came from.
type Layer struct {
	StreamKey string
	Desc      *description.Session
}

// SplitLayers builds one forwarding Layer per H264 video media found in a
// path's stream description, each paired with the (single, shared) audio
// media if present. Non-H264 video is skipped: ABR forwarding, like the
// WHEP read path, only supports a H264 ladder (see
// internal/protocols/webrtc/from_stream.go SetupFromStreamABR for the same
// constraint on the player-facing side).
//
// Video medias are expected in quality order (highest first), same
// assumption the WHEP ABR path relies on: WHIP Simulcast ingest sorts them
// by RID (internal/protocols/webrtc/to_stream.go) and RTMP multitrack
// ingest returns them sorted by track ID (gortmplib), so orig.Medias is
// already in the right order regardless of which ingest protocol was used.
func SplitLayers(streamKeyPrefix string, orig *description.Session) []Layer {
	var videoMedias []*description.Media
	var audioMedia *description.Media

	for _, media := range orig.Medias {
		switch media.Type {
		case description.MediaTypeVideo:
			for _, forma := range media.Formats {
				if _, ok := forma.(*format.H264); ok {
					videoMedias = append(videoMedias, media)
					break
				}
			}

		case description.MediaTypeAudio:
			if audioMedia == nil {
				audioMedia = media
			}
		}
	}

	layers := make([]Layer, len(videoMedias))

	for i, videoMedia := range videoMedias {
		medias := []*description.Media{videoMedia}
		if audioMedia != nil {
			medias = append(medias, audioMedia)
		}

		layers[i] = Layer{
			StreamKey: streamKeyPrefix + layerSuffix(i),
			Desc:      &description.Session{Medias: medias},
		}
	}

	return layers
}

// layerSuffixes maps quality layer index (0 = highest resolution) to its
// Tencent stream key suffix, per Tencent's naming convention. Layers beyond
// index 3 (5th and later, not expected in practice) fall back to "_qN".
var layerSuffixes = [...]string{"", "_standard", "_economic", "_lite"}

func layerSuffix(i int) string {
	if i < len(layerSuffixes) {
		return layerSuffixes[i]
	}
	return fmt.Sprintf("_q%d", i)
}
