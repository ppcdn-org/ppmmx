package webrtc

import (
	"sort"
	"time"

	webrtcproto "github.com/bluenviron/mediamtx/internal/protocols/webrtc"
)

// Server-side ABR (see also abr_ws_handler.go for the control protocol).
//
// Layer selection is driven by the packet loss the reader reports back for
// the video it received (RTCP receiver reports, aggregated per evaluation
// window by PeerConnection.OutboundVideoStats) and by RTT: RTT climbing far
// above its rolling baseline downgrades even with zero loss (throttling /
// bufferbloat queue packets instead of dropping them), and a modest RTT rise
// blocks an upgrade. It deliberately does NOT use GCC's send-side bandwidth estimate
// for the decision any more: GCC's delay-based controller only produces a
// meaningful signal when outgoing packets are paced, and this deployment
// forwards unpaced on purpose to keep latency low (see peer_connection.go's
// NoOpPacer comment). Unpaced, GCC reads every media burst (an I-frame, a
// high-motion stretch) as queue build-up and collapses to its floor, which
// is not a usable input - it demoted healthy viewers straight to the bottom
// of the ladder. Loss is a stable signal with or without pacing.
//
// GCC is still run and its estimate is still reported to the client
// (BANDWIDTH_ESTIMATE) for display, it is just no longer acted upon.
//
// The controller only acts while the reader is in automatic mode. A reader
// that has picked a layer by hand (auto=false, see SET_ABR_MODE) keeps that
// layer until it asks for something else.
const (
	// abrEvalInterval is how often loss/RTT are sampled and a switch possibly
	// made. Matches the RTCP receiver-report cadence (about once a second).
	abrEvalInterval = 1 * time.Second

	// abrWarmupPeriod is how long after the session starts decisions are
	// held off, so the first RTT baseline and loss window are available. Far
	// shorter than the old GCC-based warmup: loss is a real measurement from
	// the first reporting interval, not a constant that needs time to move.
	abrWarmupPeriod = 5 * time.Second

	// Loss thresholds, as a percentage of the packets the reader was tracked
	// as having received over one evaluation window, following the publish
	// degrade protocol's hysteresis shape (degradeRaisePct/degradeLowerPct):
	// an upgrade is considered at or below abrLossUpgradePct, a downgrade at
	// or above abrLossDowngradePct, and the band in between holds. A 1%
	// downgrade threshold (rather than a laxer 5%) reacts before a viewer
	// sees real quality loss; loss on a healthy edge-to-viewer hop is ~0.
	abrLossUpgradePct   = 0.3
	abrLossDowngradePct = 1.0

	// RTT thresholds, relative to a rolling baseline (see abrRTTWindowSamples).
	// abrRTTStableFactor gates upgrades (don't climb into a link whose RTT is
	// already rising); abrRTTDowngradeFactor triggers a downgrade even with no
	// loss, which is what bandwidth throttling and bufferbloat look like when
	// packets are queued rather than dropped - exactly the case a pure
	// loss-based rule misses. The baseline is floored at abrRTTStableMinMs so
	// sub-millisecond jitter can't make the thresholds impossibly tight.
	abrRTTStableFactor    = 1.30
	abrRTTDowngradeFactor = 2.00
	abrRTTStableMinMs     = 20.0

	// abrRTTWindowSamples is the length of the rolling RTT-minimum baseline.
	// A window rather than an all-time minimum lets the baseline follow a
	// genuine change in the link's base latency (e.g. a move to a
	// higher-latency network), instead of treating a permanently higher but
	// stable RTT as permanent congestion and downgrading forever.
	abrRTTWindowSamples = 30

	// Consecutive evaluations agreeing on a change before it is made.
	// Downgrades react faster: one layer too low costs quality, one too high
	// costs playback.
	abrDowngradeConfirmations = 2
	abrUpgradeConfirmations   = 5
)

// abrController decides which Simulcast layer a reader should receive.
// Not safe for concurrent use; it is owned by a single evaluation loop.
type abrController struct {
	selector *webrtcproto.TrackSelector

	// Confirmation counters, reset whenever the decision holds or a switch
	// is made.
	downgradeCount int
	upgradeCount   int

	// Rolling-window RTT samples and the minimum over the window, the
	// baseline the stability/congestion thresholds compare against.
	rttWindow []float64
	rttMinMs  float64
	haveRTT   bool
}

func newABRController(selector *webrtcproto.TrackSelector) *abrController {
	return &abrController{selector: selector}
}

// observeRTT folds a fresh RTT sample into the rolling-window baseline.
func (c *abrController) observeRTT(rttMs float64) {
	if rttMs <= 0 {
		return
	}
	c.rttWindow = append(c.rttWindow, rttMs)
	if len(c.rttWindow) > abrRTTWindowSamples {
		c.rttWindow = c.rttWindow[len(c.rttWindow)-abrRTTWindowSamples:]
	}
	base := c.rttWindow[0]
	for _, v := range c.rttWindow {
		if v < base {
			base = v
		}
	}
	c.rttMinMs = base
	c.haveRTT = true
}

// rttBaseline returns the rolling minimum, floored.
func (c *abrController) rttBaseline() float64 {
	base := c.rttMinMs
	if base < abrRTTStableMinMs {
		base = abrRTTStableMinMs
	}
	return base
}

// rttStable reports whether rttMs is within abrRTTStableFactor of the baseline.
// With no baseline yet it returns true rather than blocking the first upgrade.
func (c *abrController) rttStable(rttMs float64) bool {
	if !c.haveRTT || rttMs <= 0 {
		return true
	}
	return rttMs <= c.rttBaseline()*abrRTTStableFactor
}

// rttCongested reports whether rttMs is far enough above the baseline
// (abrRTTDowngradeFactor) to count as congestion on its own, even with no
// packet loss.
func (c *abrController) rttCongested(rttMs float64) bool {
	if !c.haveRTT || rttMs <= 0 {
		return false
	}
	return rttMs > c.rttBaseline()*abrRTTDowngradeFactor
}

// videoLadder returns the video tracks sorted lowest-to-highest quality
// (bitrate ascending), the order the controller steps through.
func videoLadder(tracks []webrtcproto.TrackInfo) []webrtcproto.TrackInfo {
	out := make([]webrtcproto.TrackInfo, 0, len(tracks))
	for _, t := range tracks {
		if t.Type == "video" {
			out = append(out, t)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Bitrate < out[j].Bitrate })
	return out
}

// step returns the layer `delta` steps from active in the ladder (delta -1 is
// one step down, +1 one step up), or ok=false when already at that end or the
// active layer isn't in the ladder.
func step(ladder []webrtcproto.TrackInfo, active, delta int) (int, bool) {
	idx := -1
	for i, t := range ladder {
		if t.ID == active {
			idx = i
			break
		}
	}
	if idx == -1 {
		return 0, false
	}
	next := idx + delta
	if next < 0 || next >= len(ladder) {
		return 0, false
	}
	return ladder[next].ID, true
}

// evaluate decides whether to move one layer given the loss (percent of
// packets received over the last window) and the current RTT (ms). Returns
// the target track ID and whether a switch should happen now.
func (c *abrController) evaluate(lossPct, rttMs float64) (int, bool) {
	c.observeRTT(rttMs)

	ladder := videoLadder(c.selector.GetTracks())
	if len(ladder) < 2 {
		return 0, false
	}
	active := c.selector.ActiveTrackID()

	if lossPct >= abrLossDowngradePct || c.rttCongested(rttMs) {
		c.upgradeCount = 0
		c.downgradeCount++
		if c.downgradeCount >= abrDowngradeConfirmations {
			c.downgradeCount = 0
			return step(ladder, active, -1)
		}
		return 0, false
	}

	if lossPct <= abrLossUpgradePct && c.rttStable(rttMs) {
		c.downgradeCount = 0
		c.upgradeCount++
		if c.upgradeCount >= abrUpgradeConfirmations {
			c.upgradeCount = 0
			return step(ladder, active, +1)
		}
		return 0, false
	}

	// Dead zone, or low loss with a climbing RTT: hold and drop any partial
	// streak for the other direction, so it has to be re-earned.
	c.upgradeCount = 0
	c.downgradeCount = 0
	return 0, false
}

// reset clears the confirmation counters, e.g. after a manual selection or a
// switch, so the next automatic decision starts from a clean slate.
func (c *abrController) reset() {
	c.downgradeCount = 0
	c.upgradeCount = 0
}
