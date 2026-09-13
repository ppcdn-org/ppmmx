package recorder

import (
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	rtspformat "github.com/bluenviron/gortsplib/v5/pkg/format"
)

// pickRecordedVideo returns the one video media a recording should carry,
// or nil for a stream with no video at all.
//
// Recording keeps exactly one video track. Every track a recorder adds
// shares one segment whose base timestamp comes from whichever track's
// first sample arrives first; a Simulcast publish exposes each layer as an
// independent video media with its own encoder warm-up and its own
// DTSExtractor starting from its own first IDR, so their DTS bases
// routinely disagree by more than the 1s tolerance in
// nextSegmentStartingPos. Every layer but the one that "wins" the segment
// is then permanently rejected as "received too late, discarding" (see
// formatFMP4Track.write) - not only at startup but for every later sample.
// Reconciling per-layer timestamps isn't implemented, so one track is
// chosen up front instead of letting them fight over the segment.
//
// H264 wins over any other codec, so recordings stay uniformly H264 for the
// widest downstream compatibility even on a stream that also carries a HEVC
// rendition. A stream with no H264 at all is still recorded in whatever
// codec it does carry, rather than silently producing no video.
//
// Within one codec the first media is taken, which by construction is the
// highest-quality layer: ToStream orders video medias by RID (see
// internal/protocols/webrtc/from_stream.go), so the first is RID "0".
//
// Callers report what was left out through their existing "skipping track
// N" pass, which covers every format that didn't get set up.
func pickRecordedVideo(medias []*description.Media) *description.Media {
	var selected *description.Media

	for _, media := range medias {
		if !isVideoMedia(media) {
			continue
		}
		if selected == nil {
			selected = media
		}
		if mediaHasFormat[*rtspformat.H264](media) {
			return media
		}
	}

	return selected
}

func isVideoMedia(media *description.Media) bool {
	return media.Type == description.MediaTypeVideo
}

func mediaHasFormat[T rtspformat.Format](media *description.Media) bool {
	for _, forma := range media.Formats {
		if _, ok := forma.(T); ok {
			return true
		}
	}
	return false
}
