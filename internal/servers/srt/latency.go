package srt

import (
	"sync"
	"time"

	"github.com/bluenviron/mediamtx/internal/logger"
)

// srtLatencyConfig holds the tunable parameters for latencyManager, taken
// verbatim from the conf.SRTLatency* fields - see conf.go's doc comment and
// docs/srt-adaptive-latency-design.md for the policy these implement.
type srtLatencyConfig struct {
	Initial   time.Duration // seeds a path's tuned value the first time it is seen
	Min       time.Duration
	Max       time.Duration
	Step      time.Duration // the decrease step: a below-threshold event lowers latency by this
	RaiseStep time.Duration // the increase step: an above-threshold event raises latency by this
	RaisePct  float64       // an event above this drop rate raises latency
	LowerPct  float64       // an event below this drop rate lowers latency
}

// srtLatencyPathState is one path's tuned latency.
type srtLatencyPathState struct {
	current time.Duration
	// raiseClosedAt is when this path's publisher was last force-closed to
	// apply a raise (zero = none pending). It lets the reconnecting publisher
	// measure the ingest interruption that forced reconnect cost - see
	// MarkRaiseClose / TakeRaiseCloseGap and conn.go.
	raiseClosedAt time.Time
}

// srtRaiseCloseStaleAfter bounds how long a pending raise-close timestamp is
// taken to belong to the reconnect it triggered. A publisher that only comes
// back later than this is treated as an unrelated fresh publish, so a stale
// timestamp never yields a bogus multi-minute "interruption" reading. Set well
// above any plausible OBS reconnect (which is ~1s, but can be tens of seconds
// if OBS is retrying) so a genuinely slow reconnect - exactly the case worth
// measuring - is still captured.
const srtRaiseCloseStaleAfter = 2 * time.Minute

// latencyManager owns the per-path tuned SRT receive latency. It is safe
// for concurrent use: Record is called from each connection's per-minute
// statistics goroutine (internal/servers/srt/conn.go), while LatencyFor is
// called from the server's accept loop.
type latencyManager struct {
	cfg srtLatencyConfig
	log func(logger.Level, string, ...any)

	mu    sync.Mutex
	paths map[string]*srtLatencyPathState
}

func newLatencyManager(cfg srtLatencyConfig, log func(logger.Level, string, ...any)) *latencyManager {
	return &latencyManager{
		cfg:   cfg,
		log:   log,
		paths: make(map[string]*srtLatencyPathState),
	}
}

func (m *latencyManager) logf(level logger.Level, format string, args ...any) {
	if m.log == nil {
		return
	}
	m.log(level, "SRT latency tune - "+format, args...)
}

func (m *latencyManager) getOrCreateLocked(path string) *srtLatencyPathState {
	st, ok := m.paths[path]
	if !ok {
		st = &srtLatencyPathState{current: m.cfg.Initial}
		m.paths[path] = st
	}
	return st
}

// Record applies one interval's unrecovered drop rate (percent) to path's
// tuned latency immediately: every event above cfg.RaisePct raises it by
// cfg.RaiseStep, every event below cfg.LowerPct lowers it by cfg.Step, and
// a value inside the dead band leaves it unchanged - all clamped to
// [cfg.Min, cfg.Max]. Callers must not record zero-traffic intervals
// (totalPkts == 0) - an idle interval would otherwise look like a low-drop
// event and trigger a spurious latency reduction.
//
// It returns raised == true only when this event moved the tuned value UP
// (loss above cfg.RaisePct, and the path not already pinned at cfg.Max).
// That is the signal the caller uses to force the current publisher to
// reconnect so the higher latency actually reaches the wire (see conn.go and
// docs/srt-adaptive-latency-design.md): a raise is fixing active,
// viewer-visible loss, so the reconnect pays for itself. A lower only trims
// delay off an already-healthy link and returns false - it is left to apply
// opportunistically at the path's next natural handshake rather than
// interrupting a working stream.
func (m *latencyManager) Record(path string, dropRatePct float64) (raised bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	st := m.getOrCreateLocked(path)
	next := srtLatencyNextValue(st.current, dropRatePct, m.cfg)
	if next == st.current {
		return false
	}

	m.logf(logger.Info, "path: %s, unrecovered drop: %.2f%%, latency: %v -> %v",
		path, dropRatePct, st.current, next)
	raised = next > st.current
	st.current = next
	return raised
}

// LatencyFor returns the latency a new connection on path should request.
// An unknown path is seeded with cfg.Initial (conf.SRTLatency) so it can be
// tuned from then on.
func (m *latencyManager) LatencyFor(path string) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.getOrCreateLocked(path).current
}

// MarkRaiseClose records that path's publisher was just force-closed to apply
// a raised latency, so the next publish on that path can measure and log the
// reconnect gap (see conn.go). Safe for concurrent use.
func (m *latencyManager) MarkRaiseClose(path string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getOrCreateLocked(path).raiseClosedAt = at
}

// TakeRaiseCloseGap returns the elapsed time since path's last raise-triggered
// close and clears it, reporting ok == true only when a plausible pending
// close exists (non-zero and within srtRaiseCloseStaleAfter). A path with no
// pending close, or a stale one, returns ok == false so a normal publish logs
// nothing.
func (m *latencyManager) TakeRaiseCloseGap(path string, now time.Time) (time.Duration, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.paths[path]
	if !ok || st.raiseClosedAt.IsZero() {
		return 0, false
	}
	gap := now.Sub(st.raiseClosedAt)
	st.raiseClosedAt = time.Time{}
	if gap < 0 || gap > srtRaiseCloseStaleAfter {
		return 0, false
	}
	return gap, true
}

// srtLatencyNextValue applies one event's raise/lower/hold decision to
// current and clamps the result to [cfg.Min, cfg.Max]. Extracted from
// Record so the policy can be tested without a live SRT connection.
func srtLatencyNextValue(current time.Duration, dropRatePct float64, cfg srtLatencyConfig) time.Duration {
	next := current
	switch {
	case dropRatePct > cfg.RaisePct:
		next = current + cfg.RaiseStep
	case dropRatePct < cfg.LowerPct:
		next = current - cfg.Step
	}

	if next < cfg.Min {
		next = cfg.Min
	}
	if next > cfg.Max {
		next = cfg.Max
	}
	return next
}
