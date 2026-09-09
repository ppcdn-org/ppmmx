package mpegts

import (
	"fmt"

	mcmpegts "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
	tscodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/codecs"
)

// MaxVideoLayers is the maximum number of controlled MPEG-TS simulcast layers.
const MaxVideoLayers = 4

// ValidateVideoTracks returns video tracks in the parser's order. A controlled
// sender must emit simulcast video streams highest-to-lowest; MPEG-TS codec
// objects do not carry dimensions, so quality order cannot be validated here.
// Consumers preserve this order and the corresponding PIDs.
func ValidateVideoTracks(tracks []*mcmpegts.Track) ([]*mcmpegts.Track, error) {
	var videoTracks []*mcmpegts.Track
	for _, track := range tracks {
		if track.Codec.IsVideo() {
			videoTracks = append(videoTracks, track)
		}
	}

	if len(videoTracks) <= 1 {
		return videoTracks, nil
	}

	if len(videoTracks) > MaxVideoLayers {
		return nil, fmt.Errorf("SRT publish contains %d video tracks, maximum is %d", len(videoTracks), MaxVideoLayers)
	}

	for _, track := range videoTracks {
		if _, ok := track.Codec.(*tscodecs.H264); !ok {
			return nil, fmt.Errorf("SRT publish with multiple video tracks must contain H264 only (PID %d is %T)", track.PID, track.Codec)
		}
	}

	return videoTracks, nil
}
