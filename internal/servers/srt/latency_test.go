package srt

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSRTLatencyPercentile(t *testing.T) {
	// A distribution with a heavy enough tail that it - not the mean -
	// determines the p95.
	samples := make([]float64, 0, 100)
	for range 90 {
		samples = append(samples, 0.086)
	}
	for range 10 {
		samples = append(samples, 1.73)
	}

	p95 := srtLatencyPercentile(samples, 95)
	require.InDelta(t, 1.73, p95, 0.001)

	mean := 0.0
	for _, s := range samples {
		mean += s
	}
	mean /= float64(len(samples))
	require.Less(t, mean, 0.4, "mean should sit inside the no-change dead band")
}

func TestSRTLatencyNextValue(t *testing.T) {
	cfg := srtLatencyConfig{
		Min:      400 * time.Millisecond,
		Max:      3000 * time.Millisecond,
		Step:     200 * time.Millisecond,
		RaisePct: 0.8,
		LowerPct: 0.4,
	}

	for _, ca := range []struct {
		name    string
		current time.Duration
		p95     float64
		want    time.Duration
	}{
		{"raises above threshold", 400 * time.Millisecond, 1.04, 600 * time.Millisecond},
		{"lowers below threshold", 600 * time.Millisecond, 0.2, 400 * time.Millisecond},
		{"holds in dead band", 600 * time.Millisecond, 0.6, 600 * time.Millisecond},
		{"clamps at max", 3000 * time.Millisecond, 5.0, 3000 * time.Millisecond},
		{"clamps at min", 400 * time.Millisecond, 0.0, 400 * time.Millisecond},
		{"does not undershoot min on a big step", 500 * time.Millisecond, 0.0, 400 * time.Millisecond},
	} {
		t.Run(ca.name, func(t *testing.T) {
			got := srtLatencyNextValue(ca.current, ca.p95, cfg)
			require.Equal(t, ca.want, got)
		})
	}
}

func TestSRTLatencyManagerEvaluateAll(t *testing.T) {
	cfg := srtLatencyConfig{
		Initial:      400 * time.Millisecond,
		Min:          400 * time.Millisecond,
		Max:          3000 * time.Millisecond,
		Step:         200 * time.Millisecond,
		EvalInterval: time.Hour, // not exercised directly; evaluateAll is called manually
		RaisePct:     0.8,
		LowerPct:     0.4,
		MinSamples:   60,
	}
	m := newLatencyManager(cfg, nil)

	// pathA: mostly healthy with a heavy enough bad tail that p95 (not
	// the mean) crosses the raise threshold.
	for range 90 {
		m.Record("pathA", 0.086)
	}
	for range 10 {
		m.Record("pathA", 1.73)
	}

	// pathB: consistently healthy -> lowers (starts above min so the
	// lower is observable).
	m.paths["pathB"] = &srtLatencyPathState{current: 600 * time.Millisecond}
	for range 60 {
		m.Record("pathB", 0.1)
	}

	// pathC: too few samples -> unchanged, window reset.
	for range 10 {
		m.Record("pathC", 5.0)
	}

	m.evaluateAll()

	require.Equal(t, 600*time.Millisecond, m.LatencyFor("pathA"))
	require.Equal(t, 400*time.Millisecond, m.LatencyFor("pathB"))
	require.Equal(t, 400*time.Millisecond, m.LatencyFor("pathC")) // seeded, untouched
	require.Empty(t, m.paths["pathC"].samples)
}

func TestSRTLatencyManagerZeroTrafficNotRecorded(t *testing.T) {
	// Record is only ever called by conn.go when PacketsExpected > 0 -
	// this test documents that guard's effect: an idle path that never
	// calls Record stays at its seed value indefinitely, never getting
	// pulled down by phantom "0% drop rate" samples.
	cfg := srtLatencyConfig{
		Initial:    400 * time.Millisecond,
		Min:        400 * time.Millisecond,
		Max:        3000 * time.Millisecond,
		Step:       200 * time.Millisecond,
		RaisePct:   0.8,
		LowerPct:   0.4,
		MinSamples: 60,
	}
	m := newLatencyManager(cfg, nil)

	require.Equal(t, 400*time.Millisecond, m.LatencyFor("idle"))
	m.evaluateAll()
	require.Equal(t, 400*time.Millisecond, m.LatencyFor("idle"))
}

func TestSRTLatencyManagerPathsIndependent(t *testing.T) {
	cfg := srtLatencyConfig{
		Initial:    400 * time.Millisecond,
		Min:        400 * time.Millisecond,
		Max:        3000 * time.Millisecond,
		Step:       200 * time.Millisecond,
		RaisePct:   0.8,
		LowerPct:   0.4,
		MinSamples: 10,
	}
	m := newLatencyManager(cfg, nil)

	for range 20 {
		m.Record("bad", 5.0)
		m.Record("good", 0.0)
	}
	m.paths["good"].current = 600 * time.Millisecond

	m.evaluateAll()

	require.Equal(t, 600*time.Millisecond, m.LatencyFor("bad"))
	require.Equal(t, 400*time.Millisecond, m.LatencyFor("good"))
}

func TestSRTWorstCaseBufferScalesWithLatencyMax(t *testing.T) {
	bufBytes, fc := srtWorstCaseBuffer(3000*time.Millisecond, 2*1024*1024, 2048)
	require.Greater(t, bufBytes, uint64(2*1024*1024))
	require.GreaterOrEqual(t, fc, int(bufBytes)/srtWorstCasePayloadSize)

	// never goes below the configured floor
	bufBytes, fc = srtWorstCaseBuffer(1*time.Millisecond, 2*1024*1024, 2048)
	require.Equal(t, uint64(2*1024*1024), bufBytes)
	require.Equal(t, 2048, fc)
}

func TestSRTLatencyManagerRunRespectsContext(t *testing.T) {
	cfg := srtLatencyConfig{
		Initial:      400 * time.Millisecond,
		Min:          400 * time.Millisecond,
		Max:          3000 * time.Millisecond,
		Step:         200 * time.Millisecond,
		EvalInterval: time.Millisecond,
		RaisePct:     0.8,
		LowerPct:     0.4,
		MinSamples:   1,
	}
	m := newLatencyManager(cfg, nil)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go m.Run(ctx, &wg)

	time.Sleep(5 * time.Millisecond)
	cancel()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("latencyManager.Run did not exit after context cancellation")
	}
}
