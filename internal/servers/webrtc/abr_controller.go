package webrtc

import (
	"time"

	webrtcproto "github.com/bluenviron/mediamtx/internal/protocols/webrtc"
)

// Server-side ABR (see also abr_ws_handler.go for the control protocol).
//
// Layer selection is driven by the send-side bandwidth estimate produced by
// GCC from the TWCC feedback the reader sends back
// (PeerConnection.EstimateBandwidth). This is deliberately the only input:
// decoded FPS, which used to drive the client-side engine, is a property of
// the viewer's device rather than of the link, and is now carried in
// LATENCY_REPORT for statistics only.
//
// The controller only acts while the reader is in automatic mode. A reader
// that has picked a layer by hand (auto=false, see SET_ABR_MODE) keeps that
// layer until it asks for something else - the estimate is still tracked and
// reported, just not acted upon.
const (
	// abrEvalInterval is how often the estimate is sampled and a switch
	// possibly made. Fast enough to react within a couple of seconds,
	// slow enough that each decision sees fresh TWCC feedback.
	abrEvalInterval = 1 * time.Second

	// abrWarmupPeriod is how long after the session starts the estimate is
	// ignored. GCC reports its configured initial bitrate until enough
	// feedback has arrived, so acting immediately would mean acting on a
	// constant. It also has to ride out the early over-reaction that the
	// NoOpPacer (see peer_connection.go) can provoke: without packet
	// pacing, the initial high layer is sent in bursts that GCC's delay
	// controller can misread as congestion and collapse to its floor, which
	// - via pick's "no layer fits -> lowest layer" fallback - would demote a
	// viewer straight to the bottom of the ladder. A longer warmup lets the
	// estimate recover before the first decision is made.
	abrWarmupPeriod = 30 * time.Second

	// A layer is only selected when the estimate covers its bitrate with
	// this much headroom, and is only abandoned once the estimate drops
	// below this fraction of it. The gap between the two is what keeps a
	// slowly drifting estimate from oscillating across a layer boundary.
	abrUpgradeHeadroom   = 1.20
	abrDowngradeShortage = 0.85

	// Consecutive evaluations agreeing on a change before it is made.
	// Downgrades react faster than upgrades: being one layer too low
	// costs quality, being one layer too high costs playback.
	abrDowngradeConfirmations = 2
	abrUpgradeConfirmations   = 5

	// Reserved for the audio track, which the estimate also covers but
	// which is not part of the video layer ladder.
	abrAudioReserveBits = 128_000
)

// abrController decides which simulcast layer a reader should receive.
// Not safe for concurrent use; it is owned by a single evaluation loop.
type abrController struct {
	selector *webrtcproto.TrackSelector

	// Confirmation counters, reset whenever the target agrees with the
	// current layer or a switch is made.
	downgradeCount int
	upgradeCount   int
}

func newABRController(selector *webrtcproto.TrackSelector) *abrController {
	return &abrController{selector: selector}
}

// videoBudget converts a whole-connection estimate into the share available
// to video. Returns at least zero.
func videoBudget(estimateBits int) int {
	v := estimateBits - abrAudioReserveBits
	if v < 0 {
		return 0
	}
	return v
}

// pick returns the highest layer whose bitrate fits within budget, applying
// headroom so a layer is only chosen when the link has room to spare.
// Falls back to the lowest layer when nothing fits: something has to be sent,
// and the lowest layer is the best available approximation.
func pick(tracks []webrtcproto.TrackInfo, budgetBits int) (int, bool) {
	best := -1
	bestBitrate := 0
	lowest := -1
	lowestBitrate := 0

	for _, t := range tracks {
		if t.Type != "video" {
			continue
		}
		if lowest == -1 || t.Bitrate < lowestBitrate {
			lowest = t.ID
			lowestBitrate = t.Bitrate
		}
		if float64(t.Bitrate)*abrUpgradeHeadroom > float64(budgetBits) {
			continue
		}
		if best == -1 || t.Bitrate > bestBitrate {
			best = t.ID
			bestBitrate = t.Bitrate
		}
	}

	if best != -1 {
		return best, true
	}
	if lowest != -1 {
		return lowest, true
	}
	return 0, false
}

// bitrateOf returns the configured bitrate of a video track.
func bitrateOf(tracks []webrtcproto.TrackInfo, id int) (int, bool) {
	for _, t := range tracks {
		if t.ID == id && t.Type == "video" {
			return t.Bitrate, true
		}
	}
	return 0, false
}

// evaluate decides whether to switch layers given the current estimate.
// Returns the target track ID and whether a switch should happen now.
//
// estimateBits is the whole-connection estimate in bits per second.
func (c *abrController) evaluate(estimateBits int) (int, bool) {
	tracks := c.selector.GetTracks()
	active := c.selector.ActiveTrackID()

	budget := videoBudget(estimateBits)
	target, ok := pick(tracks, budget)
	if !ok {
		return 0, false
	}

	if target == active {
		c.downgradeCount = 0
		c.upgradeCount = 0
		return 0, false
	}

	activeBitrate, haveActive := bitrateOf(tracks, active)
	targetBitrate, _ := bitrateOf(tracks, target)

	if haveActive && targetBitrate < activeBitrate {
		// Downgrade. Only act once the estimate has genuinely fallen
		// short of the current layer, not merely below the headroom the
		// upgrade rule requires - otherwise every layer whose bitrate
		// sits in that band would be abandoned as soon as it is reached.
		if float64(budget) >= float64(activeBitrate)*abrDowngradeShortage {
			c.downgradeCount = 0
			c.upgradeCount = 0
			return 0, false
		}

		c.upgradeCount = 0
		c.downgradeCount++
		if c.downgradeCount >= abrDowngradeConfirmations {
			c.downgradeCount = 0
			return target, true
		}
		return 0, false
	}

	// Upgrade (also covers the case where the active layer is unknown,
	// e.g. no switch has happened yet).
	c.downgradeCount = 0
	c.upgradeCount++
	if c.upgradeCount >= abrUpgradeConfirmations {
		c.upgradeCount = 0
		return target, true
	}
	return 0, false
}

// reset clears the confirmation counters, e.g. after a manual selection so
// the next automatic decision starts from a clean slate.
func (c *abrController) reset() {
	c.downgradeCount = 0
	c.upgradeCount = 0
}
