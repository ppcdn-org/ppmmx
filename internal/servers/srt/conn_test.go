package srt

import (
	"testing"

	"github.com/stretchr/testify/require"
)

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
