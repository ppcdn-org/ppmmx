package mpegts

import (
	"fmt"

	mcmpegts "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
	tscodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/codecs"
)

// MaxVideoLayers is the maximum number of controlled MPEG-TS simulcast layers.
const MaxVideoLayers = 4

// MaxHEVCVideoLayers caps a HEVC simulcast ladder lower than H264's, per the
// HEVC/H264 multitrack design (§11: strictly fewer than 4). Mirrors the WHIP
// server's own maxHEVCSimulcastLayers so both ingest protocols agree.
const MaxHEVCVideoLayers = 3

// VideoCodecName returns the short codec name used in path segments and log
// lines ("h264"/"hevc") for a MPEG-TS video track, or "" for a video codec
// that isn't part of the multitrack feature.
func VideoCodecName(codec mcmpegts.Codec) string {
	switch codec.(type) {
	case *tscodecs.H264:
		return "h264"
	case *tscodecs.H265:
		return "hevc"
	default:
		return ""
	}
}

// ValidateVideoTracks returns video tracks in the parser's order. A controlled
// sender must emit simulcast video streams highest-to-lowest; MPEG-TS codec
// objects do not carry dimensions, so quality order cannot be validated here.
// Consumers preserve this order and the corresponding PIDs.
//
// Every video track must use the same codec. A mixed H264+HEVC publish is
// rejected rather than accepted-and-partially-dropped: a single path can only
// carry one ABR ladder (see SetupFromStreamABR), so the layers of whichever
// codec lost would be silently discarded downstream. The HEVC/H264 multitrack
// feature instead splits the two codecs across two connections and two paths -
// see the design doc's §2.1.
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

	first := VideoCodecName(videoTracks[0].Codec)
	for _, track := range videoTracks {
		if VideoCodecName(track.Codec) != first {
			return nil, fmt.Errorf(
				"SRT publish with multiple video tracks must use a single codec "+
					"(PID %d is %T, expected %T)",
				track.PID, track.Codec, videoTracks[0].Codec)
		}
	}

	maxLayers := MaxVideoLayers
	if first == "hevc" {
		maxLayers = MaxHEVCVideoLayers
	}
	if len(videoTracks) > maxLayers {
		return nil, fmt.Errorf("SRT publish contains %d %s video tracks, maximum is %d",
			len(videoTracks), first, maxLayers)
	}

	return videoTracks, nil
}
