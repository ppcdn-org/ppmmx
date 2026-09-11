// Package recvstats computes per-interval ingest receive statistics
// (receive bitrate and packet-loss rate) from cumulative counters, so every
// publish protocol (WHIP, SRT, ...) can log the same summary in the same
// format on the same cadence. The bitrate/loss math and the log line live
// here once; each protocol only feeds in its own cumulative byte/packet
// counters.
package recvstats

import (
	"fmt"
	"time"
)

// Interval is how often each ingest connection logs its receive-stats
// summary line.
const Interval = 60 * time.Second

// Sampler turns successive cumulative counter readings into per-interval
// receive stats. It is NOT safe for concurrent use: each ingest connection
// owns one Sampler and only ever calls Sample from a single goroutine (its
// stats loop).
type Sampler struct {
	lastBytes uint64
	lastRecv  uint64
	lastLost  uint64
	lastAt    time.Time
	seeded    bool
}

// Snapshot is one interval's computed receive stats plus the cumulative
// totals at the end of the interval.
type Snapshot struct {
	BitrateBps float64       // received bits per second over the interval
	LossPct    float64       // 100 * lost / (received + lost) over the interval
	Window     time.Duration // wall-clock length of the interval

	TotalBytes    uint64 // cumulative received bytes
	TotalReceived uint64 // cumulative received packets
	TotalLost     uint64 // cumulative lost packets
}

// Sample records the current cumulative counters (received bytes, received
// packets, lost packets) and returns the stats for the interval since the
// previous Sample. The first call only establishes a baseline and returns
// ok=false; seed it once when the stream starts so the first real Sample a
// tick later covers a full interval. Counter resets (a smaller value than
// last time, e.g. a reconnect that rebuilt the socket) are treated as a
// fresh baseline rather than producing a bogus negative delta.
func (s *Sampler) Sample(bytesRecv, pktsRecv, pktsLost uint64, now time.Time) (Snapshot, bool) {
	if !s.seeded || bytesRecv < s.lastBytes || pktsRecv < s.lastRecv || pktsLost < s.lastLost {
		s.lastBytes, s.lastRecv, s.lastLost, s.lastAt, s.seeded = bytesRecv, pktsRecv, pktsLost, now, true
		return Snapshot{}, false
	}

	window := now.Sub(s.lastAt)
	dBytes := bytesRecv - s.lastBytes
	dRecv := pktsRecv - s.lastRecv
	dLost := pktsLost - s.lastLost

	var bitrate float64
	if window > 0 {
		bitrate = float64(dBytes) * 8 / window.Seconds()
	}
	var loss float64
	if denom := dRecv + dLost; denom > 0 {
		loss = float64(dLost) / float64(denom) * 100
	}

	s.lastBytes, s.lastRecv, s.lastLost, s.lastAt = bytesRecv, pktsRecv, pktsLost, now

	return Snapshot{
		BitrateBps:    bitrate,
		LossPct:       loss,
		Window:        window,
		TotalBytes:    bytesRecv,
		TotalReceived: pktsRecv,
		TotalLost:     pktsLost,
	}, true
}

// LogLine renders a Snapshot as a single consistent summary line. proto is
// the ingest protocol (e.g. "whip", "srt"); path is the stream path.
func (sn Snapshot) LogLine(proto, path string) string {
	return fmt.Sprintf(
		"[recv-stats] proto=%s path=%s bitrate=%s loss=%.2f%% (window=%s, recv=%d lost=%d bytes=%d)",
		proto, path, formatBitrate(sn.BitrateBps), sn.LossPct,
		sn.Window.Round(time.Second), sn.TotalReceived, sn.TotalLost, sn.TotalBytes,
	)
}

func formatBitrate(bps float64) string {
	switch {
	case bps >= 1e6:
		return fmt.Sprintf("%.2fMbps", bps/1e6)
	case bps >= 1e3:
		return fmt.Sprintf("%.1fkbps", bps/1e3)
	default:
		return fmt.Sprintf("%.0fbps", bps)
	}
}
