// Package degrade implements the OBS-degrade protocol's per-path state
// machine, independent of which ingest protocol (WHIP, SRT, ...) feeds it.
//
// See docs/design/publish-degrade-protocol.zh-CN.md for the full protocol.
// The ladder has two phases:
//   1. Bitrate phase (no interruption): 100% -> 80% -> 60%
//   2. Layer phase (~1s interruption): maxLayers -> maxLayers-1 -> ... -> 1
// Degrade triggers on the first sample above RaisePct (no sustained wait),
// then enters a cooldown. Recovery requires sustained compliance for
// ObservationSec.
//
// State is kept per path, not per ingest session: the OBS-side executor
// applies a degrade/recover step by restarting the publish, which creates
// a brand new session (WHIP) or connection (SRT). If state lived on the
// session/connection, every executor-triggered restart would silently
// reset it back to layers=maxLayers/bitrate=100%, and the FSM would
// immediately re-degrade, oscillating forever.
//
// Callers (one per ingest protocol) own their own sampling cadence and
// feed this package through a Manager; the WS delivery channel itself is
// served by whichever protocol server has an HTTP listener (today, only
// WebRTC's) - this package only tracks state and pushes messages to an
// already-bound *wsproto.ServerConn, it never serves the WebSocket itself.
package degrade

import (
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/mediamtx/internal/logger"
	wsproto "github.com/bluenviron/mediamtx/internal/protocols/websocket"
)

const (
	// SampleInterval is the cadence every caller SHOULD sample at. The FSM
	// treats a gap larger than StaleGap as a stale epoch (timers reset).
	SampleInterval = 6 * time.Second

	// StaleGap: a gap this large between samples means the path had no
	// active publish session sampling it for a while (not just a
	// briefly-late tick) - see State.recordSample.
	StaleGap = 5 * SampleInterval
)

// SamplePeriod returns the configured sampling period, falling back to
// SampleInterval when sec is non-positive.
func SamplePeriod(sec int) time.Duration {
	if sec <= 0 {
		return SampleInterval
	}
	return time.Duration(sec) * time.Second
}

// Thresholds is the unified trigger/debounce configuration for a protocol.
// RaisePct and LowerPct form a hysteresis band (LowerPct < RaisePct enforced
// by the caller): a sample above RaisePct triggers immediate degrade (with
// cooldown), a sample at/below LowerPct counts toward sustained recovery,
// and anything in between is a dead zone that neither degrades nor recovers.
type Thresholds struct {
	RaisePct       float64 // 0-100 - ULR above this triggers degrade
	LowerPct       float64 // 0-100 - ULR at/below this counts toward recovery
	ObservationSec int     // cooldown after degrade / sustained window for recovery
}

// TargetStateMsg is pushed on every transition and once on (re)connect -
// declarative/idempotent, not a one-shot command, so a reconnecting
// executor self-syncs without waiting for the next FSM transition.
type TargetStateMsg struct {
	Type           string `json:"type"`
	Path           string `json:"path"`
	Codec          string `json:"codec"`
	Layers         int    `json:"layers"`
	BitratePercent int    `json:"bitrate_percent"`
	LatencyMs      int    `json:"latency_ms"`
}

// codecFromPath returns the codec segment of a path name, or "" for the
// legacy/ambiguous 2-segment shape. HEVC/H264 multitrack publishes each codec
// on its own path (app/stream/h264, app/stream/hevc) with its own FSM state;
// the executor is told which codec a target state belongs to so it can apply
// bitrate/layers to the matching encoder group.
func codecFromPath(path string) string {
	switch {
	case strings.HasSuffix(path, "/h264"):
		return "h264"
	case strings.HasSuffix(path, "/hevc"):
		return "hevc"
	default:
		return ""
	}
}

// AlertMsg fires once (throttled) when the ladder has bottomed out
// (layers=1, bitrate=60%) and loss is still non-compliant.
type AlertMsg struct {
	Type   string `json:"type"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Action reports what a Sample call did to the ladder, so callers can react
// (e.g. SRT raises receiver latency on a degrade, lowers it on a recover).
type Action int

const (
	ActionNone Action = iota
	ActionDegrade
	ActionRecover
)

// State is the FSM + loss tracking for a single path. Owned by a Manager,
// looked up/created by path name; long-lived across ingest session/
// connection restarts.
type State struct {
	path string
	log  logger.Writer

	mu                   sync.Mutex
	maxLayers            int // "full"/undegraded layer count - see ObserveSessionLayerCount
	layers               int
	bitratePercent       int
	haveLastCum          bool
	lastUnrecov          uint64
	lastTotal            uint64
	goodSince            time.Time
	lastSampleAt         time.Time
	degradeCooldownUntil time.Time
	lastAlertTime        time.Time

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

// Sample feeds a fresh (cumulative unrecoverable loss, cumulative total
// packets) reading from a publish connection's stats. Cumulative counters
// reset to near-zero whenever a new ingest session/connection starts (server
// restart, executor-triggered reconnect, or a plain network drop) - such a
// reset is detected and treated as a new baseline rather than an (invalid,
// huge) negative delta. Returns the action that fired, if any.
func (d *State) Sample(cumUnrecov, cumTotal uint64, t Thresholds) Action {
	d.mu.Lock()
	if !d.haveLastCum || cumUnrecov < d.lastUnrecov || cumTotal < d.lastTotal {
		d.lastUnrecov, d.lastTotal = cumUnrecov, cumTotal
		d.haveLastCum = true
		d.mu.Unlock()
		return ActionNone
	}
	unrecovDelta := cumUnrecov - d.lastUnrecov
	totalDelta := cumTotal - d.lastTotal
	d.lastUnrecov, d.lastTotal = cumUnrecov, cumTotal
	d.mu.Unlock()

	return d.recordSample(unrecovDelta, totalDelta, t)
}

func (d *State) recordSample(unrecovDelta, totalDelta uint64, t Thresholds) Action {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()

	// Stale gap: reset observation timers after a long silence.
	if !d.lastSampleAt.IsZero() && now.Sub(d.lastSampleAt) > StaleGap {
		d.goodSince = time.Time{}
		d.degradeCooldownUntil = time.Time{}
	}
	d.lastSampleAt = now

	ulr := 0.0
	if totalDelta > 0 {
		ulr = float64(unrecovDelta) / float64(totalDelta) * 100
	}

	observation := time.Duration(t.ObservationSec) * time.Second

	// Degrade: immediate on threshold crossing, with cooldown.
	if ulr > t.RaisePct {
		d.goodSince = time.Time{}
		if !now.Before(d.degradeCooldownUntil) {
			d.degrade(now)
			d.degradeCooldownUntil = now.Add(observation)
			return ActionDegrade
		}
		return ActionNone
	}

	// Recovery: sustained compliance for ObservationSec.
	if ulr <= t.LowerPct {
		d.degradeCooldownUntil = time.Time{}
		if d.goodSince.IsZero() {
			d.goodSince = now
		} else if now.Sub(d.goodSince) >= observation {
			d.recover(now)
			d.goodSince = now
			return ActionRecover
		}
		return ActionNone
	}

	// Dead zone (LowerPct < ULR <= RaisePct): leave timers as they are.
	return ActionNone
}

// degrade and recover must be called with d.mu held.

func (d *State) degrade(now time.Time) {
	switch {
	case d.maxLayers == 0:
		return
	case d.bitratePercent > 60:
		d.bitratePercent -= 20
	case d.layers > 1:
		d.layers--
	default:
		// Terminal: layers=1, bitrate=60%, still non-compliant.
		if d.lastAlertTime.IsZero() || now.Sub(d.lastAlertTime) >= time.Duration(60)*time.Second {
			d.lastAlertTime = now
			d.log.Log(logger.Warn, "[degrade] path=%s layers=1 bitrate=60%% still non-compliant, giving up", d.path)
			d.pushLocked(AlertMsg{
				Type:   "ALERT",
				Path:   d.path,
				Reason: "layer=1,bitrate=60%,仍不合规",
			})
		}
		return
	}
	d.log.Log(logger.Info, "[degrade] path=%s -> layers=%d bitrate=%d%%", d.path, d.layers, d.bitratePercent)
	d.pushTargetStateLocked()
}

func (d *State) recover(now time.Time) {
	switch {
	case d.layers < d.maxLayers:
		d.layers++
	case d.bitratePercent < 100:
		d.bitratePercent += 20
	default:
		return
	}
	d.log.Log(logger.Info, "[degrade] path=%s recovering -> layers=%d bitrate=%d%%", d.path, d.layers, d.bitratePercent)
	d.pushTargetStateLocked()
}

func (d *State) pushTargetStateLocked() {
	d.pushLocked(TargetStateMsg{
		Type:           "TARGET_STATE",
		Path:           d.path,
		Codec:          codecFromPath(d.path),
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
		Codec:          codecFromPath(d.path),
		Layers:         d.layers,
		BitratePercent: d.bitratePercent,
	}
	d.mu.Unlock()

	if maxLayers == 0 {
		return
	}

	if err := conn.WriteJSON(msg); err != nil {
		d.log.Log(logger.Warn, "[degrade] path=%s initial WS push failed: %v", d.path, err)
	}
}

// UnbindConn clears the executor connection if it's still the current one.
func (d *State) UnbindConn(conn *wsproto.ServerConn) {
	d.wsWriteMutex.Lock()
	if d.wsConn == conn {
		d.wsConn = nil
	}
	d.wsWriteMutex.Unlock()
}
