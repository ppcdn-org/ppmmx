package webrtc

import (
	"fmt"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/mediamtx/internal/logger"
)

// TrackInfo describes an available Simulcast video track.
type TrackInfo struct {
	ID      int    `json:"id"`
	RID     string `json:"rid"`
	Type    string `json:"type"`
	Codec   string `json:"codec"`
	Label   string `json:"label"`
	Bitrate int    `json:"bitrate"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`

	dimsResolved bool // true once Width/Height/Label come from a real SPS, not layerDefaults
}

// pre-defined layer labels, used only for the TrackInfo sent to players.
// Actual pixel dimensions and bitrate are whatever the publisher is really
// sending (OBS WHIP Simulcast and RTMP multitrack encoders are user
// configurable) — these are display approximations, not negotiated values.
// RIDs match what OBS actually assigns on the wire: "0" (highest quality)
// through "3" (lowest), see plugins/obs-webrtc/whip-output.cpp and
// frontend/utility/WHIPSimulcastEncoders.hpp in the OBS Studio source.
var layerDefaults = []struct {
	rid           string
	bitrate, w, h int
}{
	{"0", 2000000, 1920, 1080},
	{"1", 1000000, 1280, 720},
	{"2", 400000, 640, 360},
	{"3", 150000, 426, 240},
}

// TrackSelector manages Simulcast track selection for a single WHEP reader.
type TrackSelector struct {
	mu         sync.RWMutex
	tracks     []*TrackInfo
	activeID   int
	pendingID  int
	pendingAt  time.Time
	onSwitched func(from, to int)

	passthrough bool // if true (Simulcast mode), forward all packets, switch immediately

	log logger.Writer
}

func NewTrackSelector(log logger.Writer, onSwitched func(from, to int)) *TrackSelector {
	return &TrackSelector{
		activeID:   -1,
		pendingID:  -1,
		onSwitched: onSwitched,
		log:        log,
	}
}

const pendingTimeout = 5 * time.Second

// LoadFromDescription loads Simulcast video tracks from the stream description.
// Supports 1–4 dynamic layers (OBS WHIP Simulcast).
// Video medias are expected in quality order (q0 → q3); ToStream orders them
// by RID on the publish side.
func (ts *TrackSelector) LoadFromDescription(desc *description.Session) error {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	ts.tracks = nil

	// Count video medias
	videoCount := 0
	for _, media := range desc.Medias {
		if media.Type == description.MediaTypeVideo {
			videoCount++
		}
	}

	// Cap at max 4 layers
	if videoCount > len(layerDefaults) {
		videoCount = len(layerDefaults)
	}

	// no fabricated layers: an audio-only stream gets no video TrackInfo
	layerCount := videoCount

	// Detect codec
	codec := "unknown"
	for _, media := range desc.Medias {
		if media.Type != description.MediaTypeVideo {
			continue
		}
		for _, forma := range media.Formats {
			switch forma.(type) {
			case *format.H264:
				codec = "h264"
			case *format.H265:
				codec = "h265"
			case *format.VP9:
				codec = "vp9"
			case *format.VP8:
				codec = "vp8"
			case *format.AV1:
				codec = "av1"
			}
			break
		}
		break
	}

	for i := 0; i < layerCount; i++ {
		l := layerDefaults[i]
		label := fmt.Sprintf("%dp %s", l.h, codec)
		ti := &TrackInfo{
			ID:      i,
			RID:     l.rid,
			Type:    "video",
			Codec:   codec,
			Label:   label,
			Bitrate: l.bitrate,
			Width:   l.w,
			Height:  l.h,
		}
		ts.tracks = append(ts.tracks, ti)
	}

	// Add audio track (ID 999). Bitrate is a display approximation, same as
	// the video layerDefaults table: Opus is variable-bitrate, so unlike
	// H264's SPS there's no fixed value to extract from the stream itself.
	// 128 kbps matches OBS's own Opus default.
	ts.tracks = append(ts.tracks, &TrackInfo{
		ID:      999,
		RID:     "",
		Type:    "audio",
		Codec:   "opus",
		Label:   "Audio Opus",
		Bitrate: 128000,
	})

	if len(ts.tracks) > 1 && ts.tracks[0].Type == "video" {
		ts.activeID = ts.tracks[0].ID
	}

	// Single video media = Simulcast mode → passthrough (no keyframe wait)
	if videoCount == 1 {
		ts.passthrough = true
	}

	return nil
}

// GetTracks returns all available tracks.
func (ts *TrackSelector) GetTracks() []TrackInfo {
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	result := make([]TrackInfo, len(ts.tracks))
	for i, t := range ts.tracks {
		result[i] = *t
	}
	return result
}

// SetTrackDimensions replaces a video track's layerDefaults-guessed
// width/height/label with the real values, decoded from the publisher's own
// SPS once its first keyframe arrives (see from_stream.go). No-op once the
// track's dimensions have already been resolved once, and on invalid input.
func (ts *TrackSelector) SetTrackDimensions(trackID, width, height int) {
	if width <= 0 || height <= 0 {
		return
	}

	ts.mu.Lock()
	defer ts.mu.Unlock()

	for _, t := range ts.tracks {
		if t.ID != trackID || t.Type != "video" || t.dimsResolved {
			continue
		}

		t.Width = width
		t.Height = height
		// the quality figure ("720p") is conventionally the shorter side,
		// so portrait streams get the same label a landscape stream at the
		// equivalent quality would.
		qualityDim := min(width, height)
		t.Label = fmt.Sprintf("%dp %s", qualityDim, t.Codec)
		t.dimsResolved = true
		return
	}
}

// VideoTrackCount returns the number of video tracks.
func (ts *TrackSelector) VideoTrackCount() int {
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	n := 0
	for _, t := range ts.tracks {
		if t.Type == "video" {
			n++
		}
	}
	return n
}

// ActiveTrackID returns the currently active track ID.
func (ts *TrackSelector) ActiveTrackID() int {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.activeID
}

// PendingTrackID returns the pending track ID (waiting for keyframe), or -1.
func (ts *TrackSelector) PendingTrackID() int {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.pendingID
}

// ShouldQueue reports whether a video track can affect the current output.
// The active track is always queued. While switching, the pending track is
// queued too so its next keyframe can complete the switch.
func (ts *TrackSelector) ShouldQueue(trackID int) bool {
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	return trackID == ts.activeID || trackID == ts.pendingID
}

// Select requests switching to the target track.
// If a switch is already pending, the new target replaces it.
func (ts *TrackSelector) Select(targetID int) error {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	found := false
	for _, t := range ts.tracks {
		if t.ID == targetID && t.Type == "video" {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("track %d not found or not a video track", targetID)
	}

	if targetID == ts.activeID {
		ts.pendingID = -1
		if ts.onSwitched != nil {
			ts.onSwitched(ts.activeID, ts.activeID)
		}
		return nil
	}

	if ts.passthrough {
		from := ts.activeID
		ts.activeID = targetID
		ts.pendingID = -1
		if ts.onSwitched != nil {
			ts.onSwitched(from, targetID)
		}
		return nil
	}

	ts.pendingID = targetID
	ts.pendingAt = time.Now()
	return nil
}

// OnAccessUnit is called for each video access unit of every subscribed track,
// with keyframe reporting whether the AU contains an IDR. It returns whether
// the AU must be forwarded to the output track. Switching happens on keyframe
// boundaries of the pending track.
func (ts *TrackSelector) OnAccessUnit(trackID int, keyframe bool) (shouldForward bool, switched bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	if ts.passthrough {
		return ts.activeID == trackID, false
	}

	if ts.pendingID >= 0 {
		if trackID == ts.pendingID && keyframe {
			from := ts.activeID
			ts.activeID = ts.pendingID
			ts.pendingID = -1

			if ts.onSwitched != nil {
				ts.onSwitched(from, ts.activeID)
			}
			return true, true
		}

		// evaluated on every access unit of every track, so a pending switch
		// cannot outlive its timeout even if the target track stops flowing
		if time.Since(ts.pendingAt) > pendingTimeout {
			cancelled := ts.pendingID
			ts.pendingID = -1

			if ts.log != nil {
				ts.log.Log(logger.Warn, "ABR pending switch to %d timed out, cancelled", cancelled)
			}
			if ts.onSwitched != nil {
				// re-confirm the current track so the client resyncs its state
				ts.onSwitched(ts.activeID, ts.activeID)
			}
		}
	}

	return trackID == ts.activeID, false
}

func (ts *TrackSelector) PipelineDelay() time.Duration {
	return 10 * time.Millisecond
}

func (ts *TrackSelector) SetLog(log logger.Writer) {
	ts.log = log
}

// SetPassthrough enables passthrough mode (Simulcast: all layers on same media).
// In passthrough mode, Select switches immediately and OnAccessUnit always
// forwards the active track without keyframe alignment.
func (ts *TrackSelector) SetPassthrough(v bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.passthrough = v
}
