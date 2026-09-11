package recvstats

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSamplerSeedThenInterval(t *testing.T) {
	base := time.Now()
	var s Sampler

	// first call only seeds a baseline
	_, ok := s.Sample(0, 0, 0, base)
	require.False(t, ok)

	// 60s later: 60 MB received => 60e6*8/60 = 8 Mbps; 10 lost of 1010 => ~0.99%
	snap, ok := s.Sample(60_000_000, 1000, 10, base.Add(60*time.Second))
	require.True(t, ok)
	require.InDelta(t, 8_000_000, snap.BitrateBps, 1)
	require.InDelta(t, 10.0/1010.0*100, snap.LossPct, 0.0001)
	require.Equal(t, 60*time.Second, snap.Window)
	require.Equal(t, uint64(60_000_000), snap.TotalBytes)
	require.Equal(t, uint64(10), snap.TotalLost)
}

func TestSamplerUsesDeltasNotCumulative(t *testing.T) {
	base := time.Now()
	var s Sampler
	s.Sample(1_000_000, 100, 0, base) // seed with a non-zero baseline

	// next interval: +30 MB over 60s = 4 Mbps; +5 lost of +505 ~= 0.99%
	snap, ok := s.Sample(31_000_000, 600, 5, base.Add(60*time.Second))
	require.True(t, ok)
	require.InDelta(t, 4_000_000, snap.BitrateBps, 1)
	require.InDelta(t, 5.0/505.0*100, snap.LossPct, 0.0001)
}

func TestSamplerNoLoss(t *testing.T) {
	base := time.Now()
	var s Sampler
	s.Sample(0, 0, 0, base)
	snap, ok := s.Sample(1_000_000, 500, 0, base.Add(60*time.Second))
	require.True(t, ok)
	require.Zero(t, snap.LossPct)
}

func TestSamplerCounterResetReseeds(t *testing.T) {
	base := time.Now()
	var s Sampler
	s.Sample(0, 0, 0, base)
	s.Sample(10_000_000, 1000, 5, base.Add(60*time.Second)) // establishes real last-values

	// a reconnect rebuilds the socket: counters drop below the last reading,
	// which must re-seed (ok=false) instead of underflowing to a huge delta.
	_, ok := s.Sample(100, 2, 0, base.Add(120*time.Second))
	require.False(t, ok)

	// and the following interval computes from that fresh baseline
	snap, ok := s.Sample(1_000_100, 502, 0, base.Add(180*time.Second))
	require.True(t, ok)
	require.InDelta(t, 1_000_000*8/60.0, snap.BitrateBps, 1)
}

func TestSnapshotLogLine(t *testing.T) {
	sn := Snapshot{BitrateBps: 5_000_000, LossPct: 0.12, Window: 60 * time.Second,
		TotalBytes: 123, TotalReceived: 45, TotalLost: 6}
	line := sn.LogLine("srt", "mystream")
	require.Contains(t, line, "[recv-stats] proto=srt path=mystream")
	require.Contains(t, line, "bitrate=5.00Mbps")
	require.Contains(t, line, "loss=0.12%")
}
