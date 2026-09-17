package srt

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnrecoverableAccumulator(t *testing.T) {
	var a unrecoverableAccumulator

	// First reading only seeds the baseline (no delta to accumulate yet).
	require.True(t, a.add(0, 0, 1000))
	require.Equal(t, uint64(0), a.unrecoverable)
	require.Equal(t, uint64(0), a.expected)

	// A normal interval: 60 lost, 50 of them retransmitted back in time,
	// 940 received => 10 unrecoverable out of 1000 expected.
	require.True(t, a.add(60, 50, 1940))
	require.Equal(t, uint64(10), a.unrecoverable)
	require.Equal(t, uint64(1000), a.expected)

	// A late retransmission lands in this interval (dRetrans > dLost): the
	// unrecoverable total must not go negative, and must not shrink.
	require.True(t, a.add(70, 70, 2930))
	require.Equal(t, uint64(10), a.unrecoverable)
	require.Equal(t, uint64(2000), a.expected)

	// A counter reset (new SRT socket) is not accumulated; totals carry over.
	require.False(t, a.add(0, 0, 500))
	require.Equal(t, uint64(10), a.unrecoverable)
	require.Equal(t, uint64(2000), a.expected)

	// Normal interval again after the reconnect.
	require.True(t, a.add(30, 10, 1000))
	require.Equal(t, uint64(30), a.unrecoverable)
	require.Equal(t, uint64(2530), a.expected)

	// The rate the FSM computes from (unrecoverable, expected-unrecoverable)
	// is unrecoverable/expected.
	rate := float64(a.unrecoverable) / float64(a.expected)
	require.InDelta(t, float64(30)/float64(2530), rate, 1e-9)
}

func TestUnrecoverableLossPct(t *testing.T) {
	for _, ca := range []struct {
		name            string
		dLost, dRetrans uint64
		packetsExpected uint64
		wantUnrecovered uint64
		wantPct         float64
	}{
		{
			name:            "no loss",
			dLost:           0,
			dRetrans:        0,
			packetsExpected: 1000,
			wantUnrecovered: 0,
			wantPct:         0,
		},
		{
			name:            "all NAKed gaps recovered by ARQ",
			dLost:           50,
			dRetrans:        50,
			packetsExpected: 1000,
			wantUnrecovered: 0,
			wantPct:         0,
		},
		{
			name:            "retrans exceeds this interval's dLost (a NAK from a prior interval landed late)",
			dLost:           10,
			dRetrans:        15,
			packetsExpected: 1000,
			wantUnrecovered: 0,
			wantPct:         0,
		},
		{
			name:            "half the detected gaps never recovered",
			dLost:           100,
			dRetrans:        40,
			packetsExpected: 1000,
			wantUnrecovered: 60,
			wantPct:         6.0,
		},
		{
			name:            "zero packetsExpected does not divide by zero",
			dLost:           10,
			dRetrans:        0,
			packetsExpected: 0,
			wantUnrecovered: 10,
			wantPct:         0,
		},
	} {
		t.Run(ca.name, func(t *testing.T) {
			unrecovered, pct := unrecoverableLossPct(ca.dLost, ca.dRetrans, ca.packetsExpected)
			require.Equal(t, ca.wantUnrecovered, unrecovered)
			require.InDelta(t, ca.wantPct, pct, 0.0001)
		})
	}
}
