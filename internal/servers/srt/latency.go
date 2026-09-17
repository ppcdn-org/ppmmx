package srt

import (
	"context"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/bluenviron/mediamtx/internal/logger"
)

// srtLatencyConfig holds the tunable parameters for latencyManager, taken
// verbatim from the conf.SRTLatency* fields - see conf.go's doc comment and
// docs/srt-adaptive-latency-design.md for the policy these implement.
type srtLatencyConfig struct {
	Initial      time.Duration // seeds a path's tuned value the first time it is seen
	Min          time.Duration
	Max          time.Duration
	Step         time.Duration
	EvalInterval time.Duration
	RaisePct     float64
	LowerPct     float64
	MinSamples   int
}

// srtLatencyPathState is one path's tuned latency and its accumulated
// unrecovered-drop-rate samples for the current evaluation window.
type srtLatencyPathState struct {
	current time.Duration
	samples []float64
}

// latencyManager owns the per-path tuned SRT receive latency. It is safe
// for concurrent use: Record is called from each connection's per-minute
// statistics goroutine (internal/servers/srt/conn.go), LatencyFor is called
// from the server's accept loop, and evaluateAll runs on its own ticker.
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

// Record appends one interval's unrecovered drop rate (percent) to path's
// current evaluation window. Callers must not record zero-traffic
// intervals (totalPkts == 0) - an idle interval would otherwise pull the
// p95 toward zero and trigger a spurious latency reduction.
func (m *latencyManager) Record(path string, dropRatePct float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.getOrCreateLocked(path)
	st.samples = append(st.samples, dropRatePct)
}

// LatencyFor returns the latency a new connection on path should request.
// An unknown path is seeded with cfg.Initial (conf.SRTLatency) and recorded
// so it can be tuned from then on.
func (m *latencyManager) LatencyFor(path string) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.getOrCreateLocked(path).current
}

// Run evaluates every known path once per cfg.EvalInterval until ctx is
// done. Intended to be started once from Server.Initialize.
func (m *latencyManager) Run(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()

	ticker := time.NewTicker(m.cfg.EvalInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.evaluateAll()
		case <-ctx.Done():
			return
		}
	}
}

func (m *latencyManager) evaluateAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for path, st := range m.paths {
		samples := st.samples
		st.samples = nil

		if len(samples) < m.cfg.MinSamples {
			m.logf(logger.Info, "path: %s, skipped: insufficient samples: %d < %d",
				path, len(samples), m.cfg.MinSamples)
			continue
		}

		p95 := srtLatencyPercentile(samples, 95)
		next := srtLatencyNextValue(st.current, p95, m.cfg)

		if next == st.current {
			m.logf(logger.Info, "path: %s, samples: %d, p95: %.2f%%, latency: %v (unchanged)",
				path, len(samples), p95, st.current)
			continue
		}

		m.logf(logger.Info, "path: %s, samples: %d, p95: %.2f%%, latency: %v -> %v",
			path, len(samples), p95, st.current, next)
		st.current = next
	}
}

// srtLatencyPercentile returns the p-th percentile (0-100) of samples using
// the nearest-rank method. samples is not mutated.
func srtLatencyPercentile(samples []float64, p float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)
	idx := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// srtLatencyNextValue applies one evaluation's raise/lower/hold decision to
// current and clamps the result to [cfg.Min, cfg.Max]. Extracted from
// evaluateAll so the policy can be tested without a live SRT connection.
func srtLatencyNextValue(current time.Duration, p95 float64, cfg srtLatencyConfig) time.Duration {
	next := current
	switch {
	case p95 > cfg.RaisePct:
		next = current + cfg.Step
	case p95 < cfg.LowerPct:
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
