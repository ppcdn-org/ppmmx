// Package degrade implements the OBS-degrade protocol's per-path state
// machine, independent of which ingest protocol (WHIP, SRT, ...) feeds it.
//
// See docs/obs-mmx-degrade-protocol.md for the full protocol: on sustained
// RTP/packet loss on a publish, mmx degrades simulcast layers (3->2->1)
// then bitrate (100%->80%) in single steps, pushing the resulting target
// state to a per-path WebSocket that an OBS-side executor connects to.
// Recovery walks the same steps in reverse (bitrate first, then layers)
// once loss is compliant again.
//
// State is kept per path, not per ingest session: the OBS-side executor
// applies a degrade/recover step by restarting the publish, which creates
// a brand new session (WHIP) or connection (SRT). If state lived on the
// session/connection, every executor-triggered restart would silently
// reset it back to layers=3/bitrate=100%, and the FSM would immediately
// re-degrade, oscillating forever.
//
// Callers (one per ingest protocol) own their own sampling cadence/
// threshold config and feed this package through a Manager; the WS
// delivery channel itself is served by whichever protocol server has an
// HTTP listener (today, only WebRTC's) - this package only tracks state
// and pushes messages to an already-bound *wsproto.ServerConn, it never
// serves the WebSocket itself.
package degrade

import (
	"sync"
	"time"

	"github.com/bluenviron/mediamtx/internal/logger"
	wsproto "github.com/bluenviron/mediamtx/internal/protocols/websocket"
)

const (
	// SampleInterval is the cadence every caller MUST sample at. AvgWindowSize
	// below is a sample COUNT, not a duration - it only means "5 minutes" if
	// every caller samples at exactly this interval.
	SampleInterval = 1 * time.Second
	AvgWindowSize  = 300 // 5 minutes at SampleInterval

	// StaleGap: a gap this large between samples means the path had no
	// active publish session sampling it for a while (not just a
	// briefly-late tick) - see State.recordSample.
	StaleGap = 5 * SampleInterval
)

// Thresholds is one protocol's trigger/debounce configuration. WHIP and SRT
// each pass their own value on every call rather than it being stored on
// State/Manager, so the same shared per-path state can react differently
// depending on which protocol most recently fed it a sample, with neither
// side's config ever silently overwriting the other's.
//
// Degrade* and Recover* are deliberately separate (hysteresis/Schmitt
// trigger): a sample above Degrade* counts toward degrading, a sample at or
// below Recover* counts toward recovering, and anything strictly in between
// (Recover* < loss <= Degrade*, whenever Recover* < Degrade*) is a dead zone
// that does neither - it freezes whatever badSince/goodSince timer was
// already running instead of resetting it. Without this gap, a loss rate
// hovering right at a single shared threshold can flip badSince/goodSince
// back and forth every sample and oscillate the ladder step forever once
// ObservationSec elapses on either side. Setting Recover* == Degrade*
// collapses the dead zone to zero width, reproducing the old single-
// threshold behavior exactly.
type Thresholds struct {
	DegradeInstantLossPct float64 // 0-100 - sustained loss ABOVE this degrades
	DegradeAvgLossPct     float64 // 0-100
	RecoverInstantLossPct float64 // 0-100 - loss must stay AT/BELOW this to recover
	RecoverAvgLossPct     float64 // 0-100
	ObservationSec        int
}

// TargetStateMsg is pushed on every transition and once on (re)connect -
// declarative/idempotent, not a one-shot command, so a reconnecting
// executor self-syncs without waiting for the next FSM transition.
type TargetStateMsg struct {
	Type           string `json:"type"`
	Path           string `json:"path"`
	Layers         int    `json:"layers"`
	BitratePercent int    `json:"bitrate_percent"`
}

// AlertMsg fires once (throttled) when the ladder has bottomed out
// (layers=1, bitrate=80%) and loss is still non-compliant.
type AlertMsg struct {
	Type   string `json:"type"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// lossWindow is a fixed-size ring buffer of per-sample (lost, received)
// deltas, used to compute a trailing average loss rate.
type lossWindow struct {
	lost, received [AvgWindowSize]uint64
	idx            int
	filled         bool
}

func (w *lossWindow) add(lostDelta, receivedDelta uint64) {
	w.lost[w.idx] = lostDelta
	w.received[w.idx] = receivedDelta
	w.idx++
	if w.idx == AvgWindowSize {
		w.idx = 0
		w.filled = true
	}
}

func (w *lossWindow) averageLossRate() float64 {
	n := w.idx
	if w.filled {
		n = AvgWindowSize
	}
	var lost, total uint64
	for i := 0; i < n; i++ {
		lost += w.lost[i]
		total += w.lost[i] + w.received[i]
	}
	if total == 0 {
		return 0
	}
	return float64(lost) / float64(total)
}

// State is the FSM + loss tracking for a single path. Owned by a Manager,
// looked up/created by path name; long-lived across ingest session/
// connection restarts.
type State struct {
	path string
	log  logger.Writer

	mu             sync.Mutex
	maxLayers      int // "full"/undegraded layer count - see ObserveSessionLayerCount
	layers         int
	bitratePercent int
	window         lossWindow
	haveLastCum    bool
	lastLost       uint64
	lastReceived   uint64
	badSince       time.Time
	goodSince      time.Time
	lastSampleAt   time.Time
	lastAlertTime  time.Time

	wsWriteMutex sync.Mutex
	wsConn       *wsproto.ServerConn
}

func newState(path string, log logger.Writer) *State {
	return &State{
		path:           path,
		log:            log,
		bitratePercent: 100,
		// maxLayers/layers start at 0 ("unknown") until the first publish
		// session reports how many layers it actually negotiated - see
		// ObserveSessionLayerCount. Different OBS profiles/protocols
		// negotiate different real layer counts, so this can't be a fixed
		// constant.
	}
}

// ObserveSessionLayerCount is called once per new publish session/
// connection with the number of layers it actually negotiated (WHIP: inbound
// video track count; SRT: multiplexed video track count). It only ever
// grows maxLayers, never shrinks it: a degrade-triggered restart
// intentionally reconnects with fewer real layers than maxLayers (that's
// the whole point - see the package doc comment on why state must survive
// that), and must not be mistaken for the deployment's capacity shrinking.
// A real capacity *increase* is treated as a fresh full/undegraded starting
// point.
func (d *State) ObserveSessionLayerCount(realLayers int) {
	if realLayers <= 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if realLayers > d.maxLayers {
		d.maxLayers = realLayers
		d.layers = realLayers
		d.bitratePercent = 100
		d.log.Log(logger.Info, "[degrade] path=%s max layers set to %d", d.path, d.maxLayers)
		d.pushTargetStateLocked()
	}
}

// Sample feeds a fresh (cumulative lost, cumulative received) reading from
// a publish connection's stats. Cumulative counters reset to near-zero
// whenever a new ingest session/connection starts (server restart,
// executor-triggered reconnect, or a plain network drop) - such a reset is
// detected and treated as a new baseline rather than an (invalid, huge)
// negative delta.
func (d *State) Sample(cumLost, cumReceived uint64, t Thresholds) {
	d.mu.Lock()
	if !d.haveLastCum || cumLost < d.lastLost || cumReceived < d.lastReceived {
		d.lastLost, d.lastReceived = cumLost, cumReceived
		d.haveLastCum = true
		d.mu.Unlock()
		return
	}
	lostDelta := cumLost - d.lastLost
	receivedDelta := cumReceived - d.lastReceived
	d.lastLost, d.lastReceived = cumLost, cumReceived
	d.mu.Unlock()

	d.recordSample(lostDelta, receivedDelta, t)
}

func (d *State) recordSample(lostDelta, receivedDelta uint64, t Thresholds) {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()

	// If nothing has sampled this path in a while - no active publish
	// session, e.g. between a dropped connection and its eventual
	// reconnect - badSince/goodSince stop describing a continuously
	// observed condition, and the average window holds data from a
	// different, no-longer-relevant network circumstance. Start a fresh
	// observation epoch: reconnecting after a long silence must earn its
	// own ObservationSec of continuous (non-)compliance, not inherit
	// whatever the state happened to be when sampling last stopped. This
	// does NOT touch layers/bitratePercent - those intentionally persist
	// across reconnects (see the package doc comment).
	if !d.lastSampleAt.IsZero() && now.Sub(d.lastSampleAt) > StaleGap {
		d.badSince = time.Time{}
		d.goodSince = time.Time{}
		d.window = lossWindow{}
	}
	d.lastSampleAt = now

	instant := 0.0
	if total := lostDelta + receivedDelta; total > 0 {
		instant = float64(lostDelta) / float64(total)
	}
	d.window.add(lostDelta, receivedDelta)
	avg := d.window.averageLossRate()

	good := instant <= t.RecoverInstantLossPct/100 && avg <= t.RecoverAvgLossPct/100
	bad := instant > t.DegradeInstantLossPct/100 || avg > t.DegradeAvgLossPct/100
	observation := time.Duration(t.ObservationSec) * time.Second

	switch {
	case good:
		d.badSince = time.Time{}
		if d.goodSince.IsZero() {
			d.goodSince = now
		}
		if now.Sub(d.goodSince) >= observation {
			d.recover(now)
			d.goodSince = now
		}

	case bad:
		d.goodSince = time.Time{}
		if d.badSince.IsZero() {
			d.badSince = now
		}
		if now.Sub(d.badSince) >= observation {
			d.degrade(now, observation)
			d.badSince = now
		}

	default:
		// Dead zone (Recover* < loss <= Degrade*): neither good nor bad
		// enough to count. Leave badSince/goodSince exactly as they are -
		// a timer already running keeps accumulating in wall-clock time
		// toward its own threshold, it just isn't reset or advanced by
		// this particular sample.
	}
}

// degrade and recover must be called with d.mu held.

func (d *State) degrade(now time.Time, observation time.Duration) {
	switch {
	case d.maxLayers == 0:
		// No session has reported its real layer count yet - nothing
		// meaningful to degrade.
		return
	case d.layers > 1:
		d.layers--
	case d.layers == 1 && d.bitratePercent == 100:
		d.bitratePercent = 80
	default:
		// Terminal state: layers=1, bitrate=80%, still non-compliant.
		// No further automatic action - alert (throttled) and stop.
		if d.lastAlertTime.IsZero() || now.Sub(d.lastAlertTime) >= observation {
			d.lastAlertTime = now
			d.log.Log(logger.Warn, "[degrade] path=%s layers=1 bitrate=80%% still non-compliant, giving up", d.path)
			d.pushLocked(AlertMsg{
				Type:   "ALERT",
				Path:   d.path,
				Reason: "layer=1,bitrate=80%,仍不合规",
			})
		}
		return
	}
	d.log.Log(logger.Info, "[degrade] path=%s -> layers=%d bitrate=%d%%", d.path, d.layers, d.bitratePercent)
	d.pushTargetStateLocked()
}

func (d *State) recover(now time.Time) {
	switch {
	case d.bitratePercent == 80:
		d.bitratePercent = 100
	case d.layers < d.maxLayers:
		d.layers++
	default:
		return // already fully recovered (or maxLayers still unknown)
	}
	d.log.Log(logger.Info, "[degrade] path=%s recovering -> layers=%d bitrate=%d%%", d.path, d.layers, d.bitratePercent)
	d.pushTargetStateLocked()
}

func (d *State) pushTargetStateLocked() {
	d.pushLocked(TargetStateMsg{
		Type:           "TARGET_STATE",
		Path:           d.path,
		Layers:         d.layers,
		BitratePercent: d.bitratePercent,
	})
}

func (d *State) pushLocked(msg any) {
	d.wsWriteMutex.Lock()
	conn := d.wsConn
	d.wsWriteMutex.Unlock()
	if conn == nil {
		return
	}
	if err := conn.WriteJSON(msg); err != nil {
		d.log.Log(logger.Warn, "[degrade] path=%s WS push failed: %v", d.path, err)
	}
}

// BindConn registers conn as this path's current executor connection,
// closing any previous one, and immediately sends the current target state
// so a (re)connecting executor self-syncs instead of waiting for the next
// FSM transition.
func (d *State) BindConn(conn *wsproto.ServerConn) {
	d.wsWriteMutex.Lock()
	previous := d.wsConn
	d.wsConn = conn
	d.wsWriteMutex.Unlock()
	if previous != nil && previous != conn {
		previous.Close()
	}

	d.mu.Lock()
	maxLayers := d.maxLayers
	msg := TargetStateMsg{
		Type:           "TARGET_STATE",
		Path:           d.path,
		Layers:         d.layers,
		BitratePercent: d.bitratePercent,
	}
	d.mu.Unlock()

	if maxLayers == 0 {
		// No publish session has reported its real layer count yet -
		// nothing meaningful to sync the executor to.
		return
	}

	if err := conn.WriteJSON(msg); err != nil {
		d.log.Log(logger.Warn, "[degrade] path=%s initial WS push failed: %v", d.path, err)
	}
}

// UnbindConn clears the executor connection if it's still the current one
// (a newer connection may have already replaced it via BindConn).
func (d *State) UnbindConn(conn *wsproto.ServerConn) {
	d.wsWriteMutex.Lock()
	if d.wsConn == conn {
		d.wsConn = nil
	}
	d.wsWriteMutex.Unlock()
}
