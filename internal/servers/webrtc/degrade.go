package webrtc

import (
	"sync"
	"time"

	"github.com/bluenviron/mediamtx/internal/logger"
	wsproto "github.com/bluenviron/mediamtx/internal/protocols/websocket"
)

// See docs/obs-mmx-degrade-protocol.md for the full protocol this
// implements: on sustained RTP loss on a WHIP (publish) session, mmx
// degrades simulcast layers (3->2->1) then bitrate (100%->80%) in single
// steps, pushing the resulting target state to a per-path WebSocket that
// an OBS-side executor connects to. Recovery walks the same steps in
// reverse (bitrate first, then layers) once loss is compliant again.
//
// State is kept per path, not per WHIP session: the OBS-side executor
// applies a degrade/recover step by restarting the WHIP push, which
// creates a brand new session. If state lived on the session, every
// executor-triggered restart would silently reset it back to
// layers=3/bitrate=100%, and the FSM would immediately re-degrade,
// oscillating forever.
const (
	degradeSampleInterval = 1 * time.Second
	degradeAvgWindowSize  = 300 // 5 minutes at 1 sample/s

	// degradeStaleGap: a gap this large between samples means the path had
	// no active publish session sampling it for a while (not just a
	// briefly-late tick) - see recordSample.
	degradeStaleGap = 5 * degradeSampleInterval
)

type degradeTargetStateMsg struct {
	Type           string `json:"type"`
	Path           string `json:"path"`
	Layers         int    `json:"layers"`
	BitratePercent int    `json:"bitrate_percent"`
}

type degradeAlertMsg struct {
	Type   string `json:"type"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// lossWindow is a fixed-size ring buffer of per-sample (lost, received)
// deltas, used to compute a trailing average loss rate.
type lossWindow struct {
	lost, received [degradeAvgWindowSize]uint64
	idx            int
	filled         bool
}

func (w *lossWindow) add(lostDelta, receivedDelta uint64) {
	w.lost[w.idx] = lostDelta
	w.received[w.idx] = receivedDelta
	w.idx++
	if w.idx == degradeAvgWindowSize {
		w.idx = 0
		w.filled = true
	}
}

func (w *lossWindow) averageLossRate() float64 {
	n := w.idx
	if w.filled {
		n = degradeAvgWindowSize
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

// degradeState is the FSM + loss tracking for a single path. Owned by
// Server, looked up/created by path name; long-lived across WHIP session
// restarts.
type degradeState struct {
	path   string
	server *Server

	mu             sync.Mutex
	maxLayers      int // "full"/undegraded layer count - see observeSessionLayerCount
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

func newDegradeState(path string, server *Server) *degradeState {
	return &degradeState{
		path:           path,
		server:         server,
		bitratePercent: 100,
		// maxLayers/layers start at 0 ("unknown") until the first publish
		// session reports how many simulcast layers it actually
		// negotiated - see observeSessionLayerCount. Different OBS
		// profiles negotiate different real layer counts (e.g. 4 for
		// 1080p, 3 for 720p), so this can no longer be a fixed constant.
	}
}

// observeSessionLayerCount is called once per new publish session with the
// number of video layers it actually negotiated (len of inbound video
// tracks). It only ever grows maxLayers, never shrinks it: a
// degrade-triggered restart intentionally reconnects with fewer real
// layers than maxLayers (that's the whole point - see the package doc
// comment on why state must survive that), and must not be mistaken for
// the deployment's capacity shrinking. A real capacity *increase* (e.g.
// switching the OBS profile from 3-layer 720p to 4-layer 1080p) is
// treated as a fresh full/undegraded starting point.
func (d *degradeState) observeSessionLayerCount(realLayers int) {
	if realLayers <= 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if realLayers > d.maxLayers {
		d.maxLayers = realLayers
		d.layers = realLayers
		d.bitratePercent = 100
		d.server.Log(logger.Info, "[degrade] path=%s max layers set to %d", d.path, d.maxLayers)
		d.pushTargetStateLocked()
	}
}

// sample feeds a fresh (cumulative lost, cumulative received) reading from
// a publish session's RTP stats. Cumulative counters reset to near-zero
// whenever a new WHIP session starts (server restart, executor-triggered
// reconnect, or a plain network drop) - such a reset is detected and
// treated as a new baseline rather than an (invalid, huge) negative delta.
func (d *degradeState) sample(cumLost, cumReceived uint64) {
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

	d.recordSample(lostDelta, receivedDelta)
}

func (d *degradeState) recordSample(lostDelta, receivedDelta uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()

	// If nothing has sampled this path in a while - no active publish
	// session, e.g. between a dropped WHIP connection and its eventual
	// reconnect - badSince/goodSince stop describing a continuously
	// observed condition, and the average window holds data from a
	// different, no-longer-relevant network circumstance. Start a fresh
	// observation epoch: reconnecting after a long silence must earn its
	// own 60s of continuous (non-)compliance, not inherit whatever the
	// state happened to be when sampling last stopped. This does NOT
	// touch layers/bitratePercent - those intentionally persist across
	// reconnects (see the package doc comment).
	if !d.lastSampleAt.IsZero() && now.Sub(d.lastSampleAt) > degradeStaleGap {
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

	compliant := instant <= d.server.DegradeInstantLossPct/100 && avg <= d.server.DegradeAvgLossPct/100
	observation := time.Duration(d.server.DegradeObservationSec) * time.Second

	if compliant {
		d.badSince = time.Time{}
		if d.goodSince.IsZero() {
			d.goodSince = now
		}
		if now.Sub(d.goodSince) >= observation {
			d.recover(now)
			d.goodSince = now
		}
		return
	}

	d.goodSince = time.Time{}
	if d.badSince.IsZero() {
		d.badSince = now
	}
	if now.Sub(d.badSince) >= observation {
		d.degrade(now, observation)
		d.badSince = now
	}
}

// degrade and recover must be called with d.mu held.

func (d *degradeState) degrade(now time.Time, observation time.Duration) {
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
			d.server.Log(logger.Warn, "[degrade] path=%s layers=1 bitrate=80%% still non-compliant, giving up", d.path)
			d.pushLocked(degradeAlertMsg{
				Type:   "ALERT",
				Path:   d.path,
				Reason: "layer=1,bitrate=80%,仍不合规",
			})
		}
		return
	}
	d.server.Log(logger.Info, "[degrade] path=%s -> layers=%d bitrate=%d%%", d.path, d.layers, d.bitratePercent)
	d.pushTargetStateLocked()
}

func (d *degradeState) recover(now time.Time) {
	switch {
	case d.bitratePercent == 80:
		d.bitratePercent = 100
	case d.layers < d.maxLayers:
		d.layers++
	default:
		return // already fully recovered (or maxLayers still unknown)
	}
	d.server.Log(logger.Info, "[degrade] path=%s recovering -> layers=%d bitrate=%d%%", d.path, d.layers, d.bitratePercent)
	d.pushTargetStateLocked()
}

func (d *degradeState) pushTargetStateLocked() {
	d.pushLocked(degradeTargetStateMsg{
		Type:           "TARGET_STATE",
		Path:           d.path,
		Layers:         d.layers,
		BitratePercent: d.bitratePercent,
	})
}

func (d *degradeState) pushLocked(msg any) {
	d.wsWriteMutex.Lock()
	conn := d.wsConn
	d.wsWriteMutex.Unlock()
	if conn == nil {
		return
	}
	if err := conn.WriteJSON(msg); err != nil {
		d.server.Log(logger.Warn, "[degrade] path=%s WS push failed: %v", d.path, err)
	}
}

// bindConn registers conn as this path's current executor connection,
// closing any previous one, and immediately sends the current target
// state so a (re)connecting executor self-syncs instead of waiting for
// the next FSM transition.
func (d *degradeState) bindConn(conn *wsproto.ServerConn) {
	d.wsWriteMutex.Lock()
	previous := d.wsConn
	d.wsConn = conn
	d.wsWriteMutex.Unlock()
	if previous != nil && previous != conn {
		previous.Close()
	}

	d.mu.Lock()
	maxLayers := d.maxLayers
	msg := degradeTargetStateMsg{
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
		d.server.Log(logger.Warn, "[degrade] path=%s initial WS push failed: %v", d.path, err)
	}
}

// unbindConn clears the executor connection if it's still the current one
// (a newer connection may have already replaced it via bindConn).
func (d *degradeState) unbindConn(conn *wsproto.ServerConn) {
	d.wsWriteMutex.Lock()
	if d.wsConn == conn {
		d.wsConn = nil
	}
	d.wsWriteMutex.Unlock()
}

// getOrCreateDegradeState returns the degrade FSM for path, creating it on
// first use. Plain mutex-guarded map, independent of the session-manager
// actor loop: degrade state outlives individual WHIP sessions and doesn't
// need to interact with session create/close events.
func (s *Server) getOrCreateDegradeState(path string) *degradeState {
	s.degradeStatesMu.Lock()
	defer s.degradeStatesMu.Unlock()
	if ds, ok := s.degradeStates[path]; ok {
		return ds
	}
	ds := newDegradeState(path, s)
	s.degradeStates[path] = ds
	return ds
}

// recordDegradeSample implements the sessionParent hook used by
// session.runDegradeSampling.
func (s *Server) recordDegradeSample(pathName string, cumLost, cumReceived uint64) {
	s.getOrCreateDegradeState(pathName).sample(cumLost, cumReceived)
}

// observeDegradeSessionLayers implements the sessionParent hook used by
// session.runDegradeSampling to report a fresh session's real negotiated
// simulcast layer count.
func (s *Server) observeDegradeSessionLayers(pathName string, realLayers int) {
	s.getOrCreateDegradeState(pathName).observeSessionLayerCount(realLayers)
}

// degradeSampleEnabled implements the sessionParent hook.
func (s *Server) degradeSampleEnabled() bool {
	return s.DegradeEnable
}
