package srt

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSRTLatencyNextValue(t *testing.T) {
	cfg := srtLatencyConfig{
		Initial:  500 * time.Millisecond,
		Min:      300 * time.Millisecond,
		Max:      3000 * time.Millisecond,
		Step:     100 * time.Millisecond,
		RaisePct: 1.0,
		LowerPct: 0.1,
	}

	for _, ca := range []struct {
		name    string
		current time.Duration
		dropPct float64
		want    time.Duration
	}{
		{"raises above threshold", 500 * time.Millisecond, 1.01, 600 * time.Millisecond},
		{"holds exactly at raise threshold", 500 * time.Millisecond, 1.0, 500 * time.Millisecond},
		{"lowers below threshold", 500 * time.Millisecond, 0.09, 400 * time.Millisecond},
		{"holds exactly at lower threshold", 500 * time.Millisecond, 0.1, 500 * time.Millisecond},
		{"holds in dead band", 500 * time.Millisecond, 0.5, 500 * time.Millisecond},
		{"clamps at max", 3000 * time.Millisecond, 5.0, 3000 * time.Millisecond},
		{"clamps at min", 300 * time.Millisecond, 0.0, 300 * time.Millisecond},
		{"does not undershoot min on a big step", 350 * time.Millisecond, 0.0, 300 * time.Millisecond},
		{"does not overshoot max on a big step", 2950 * time.Millisecond, 5.0, 3000 * time.Millisecond},
	} {
		t.Run(ca.name, func(t *testing.T) {
			got := srtLatencyNextValue(ca.current, ca.dropPct, cfg)
			require.Equal(t, ca.want, got)
		})
	}
}

func TestSRTLatencyManagerRecordAdjustsPerEvent(t *testing.T) {
	cfg := srtLatencyConfig{
		Initial:  500 * time.Millisecond,
		Min:      300 * time.Millisecond,
		Max:      3000 * time.Millisecond,
		Step:     100 * time.Millisecond,
		RaisePct: 1.0,
		LowerPct: 0.1,
	}
	m := newLatencyManager(cfg, nil)

	require.Equal(t, 500*time.Millisecond, m.LatencyFor("path"))

	// every >1% event raises by one step
	m.Record("path", 1.5)
	require.Equal(t, 600*time.Millisecond, m.LatencyFor("path"))
	m.Record("path", 1.5)
	require.Equal(t, 700*time.Millisecond, m.LatencyFor("path"))

	// dead band holds
	m.Record("path", 0.5)
	require.Equal(t, 700*time.Millisecond, m.LatencyFor("path"))

	// every <0.1% event lowers by one step
	m.Record("path", 0.05)
	require.Equal(t, 600*time.Millisecond, m.LatencyFor("path"))
}

func TestSRTLatencyManagerClampsToBounds(t *testing.T) {
	cfg := srtLatencyConfig{
		Initial:  500 * time.Millisecond,
		Min:      300 * time.Millisecond,
		Max:      3000 * time.Millisecond,
		Step:     100 * time.Millisecond,
		RaisePct: 1.0,
		LowerPct: 0.1,
	}
	m := newLatencyManager(cfg, nil)

	for range 100 {
		m.Record("bad", 5.0)
	}
	require.Equal(t, cfg.Max, m.LatencyFor("bad"))

	for range 100 {
		m.Record("good", 0.0)
	}
	require.Equal(t, cfg.Min, m.LatencyFor("good"))
}

func TestSRTLatencyManagerPathsIndependent(t *testing.T) {
	cfg := srtLatencyConfig{
		Initial:  500 * time.Millisecond,
		Min:      300 * time.Millisecond,
		Max:      3000 * time.Millisecond,
		Step:     100 * time.Millisecond,
		RaisePct: 1.0,
		LowerPct: 0.1,
	}
	m := newLatencyManager(cfg, nil)

	m.Record("bad", 5.0)
	m.Record("good", 0.0)

	require.Equal(t, 600*time.Millisecond, m.LatencyFor("bad"))
	require.Equal(t, 400*time.Millisecond, m.LatencyFor("good"))
}

func TestSRTLatencyManagerUnknownPathSeeded(t *testing.T) {
	// A path that is seen only by LatencyFor (a connection accepted before
	// any statistics sample exists) reports the seed value; without a
	// Record call it is never tuned, so an idle path is never pulled down
	// by phantom "0% drop rate" samples.
	cfg := srtLatencyConfig{
		Initial:  500 * time.Millisecond,
		Min:      300 * time.Millisecond,
		Max:      3000 * time.Millisecond,
		Step:     100 * time.Millisecond,
		RaisePct: 1.0,
		LowerPct: 0.1,
	}
	m := newLatencyManager(cfg, nil)

	require.Equal(t, 500*time.Millisecond, m.LatencyFor("idle"))
	require.Equal(t, 500*time.Millisecond, m.LatencyFor("idle"))
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
