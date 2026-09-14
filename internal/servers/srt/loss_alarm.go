package srt

import (
	"context"
	"time"
)

// sustainedLossTracker turns successive recvstats.Snapshot.LossPct samples
// (one every recvstats.Interval) into a raise/refresh/resolve decision plus
// a continuous-over-threshold duration, without knowing anything about
// sockets or HTTP. Not safe for concurrent use - one tracker per SRT publish
// connection, driven only from that connection's own stats goroutine.
type sustainedLossTracker struct {
	exceededSince time.Time
	wasOver       bool
}

// lossAlarmDecision is what to do with one sample.
type lossAlarmDecision struct {
	// ShouldReport is true on every sample while loss stays over threshold
	// (so ppcenter's alarm stays fresh/active), and exactly once more on the
	// sample where it drops back under threshold (the resolve report).
	ShouldReport bool
	// Sustained is how long loss has been continuously over threshold as of
	// this sample; zero when not currently over.
	Sustained time.Duration
	// ShouldDisconnect is true once Sustained has reached disconnectAfter,
	// for as long as the caller keeps calling update (the caller is
	// expected to act on the first true and stop calling, since the
	// connection is being torn down).
	ShouldDisconnect bool
}

// update folds in one new sample. thresholdPct and disconnectAfter are
// re-read from config on every call rather than captured once, so a
// mid-connection config reload takes effect on the next sample.
func (t *sustainedLossTracker) update(lossPct, thresholdPct float64, disconnectEnable bool, disconnectAfter time.Duration, now time.Time) lossAlarmDecision {
	over := lossPct > thresholdPct
	if !over {
		wasOver := t.wasOver
		t.exceededSince, t.wasOver = time.Time{}, false
		return lossAlarmDecision{ShouldReport: wasOver}
	}

	if t.exceededSince.IsZero() {
		t.exceededSince = now
	}
	t.wasOver = true
	sustained := now.Sub(t.exceededSince)

	return lossAlarmDecision{
		ShouldReport:     true,
		Sustained:        sustained,
		ShouldDisconnect: disconnectEnable && sustained >= disconnectAfter,
	}
}

// srtLossAlarmReporter is the subset of mmxcontrol.SRTLossAlarmClient that
// runReceiveStatsSummary needs, narrowed to an interface so this package
// doesn't import mmxcontrol (matching how recording's SplitRecFileReporter
// decouples internal/servers packages from internal/mmxcontrol) - internal/
// core bridges the two.
type srtLossAlarmReporter interface {
	ReportSRTLoss(ctx context.Context, pathName string, lossPct, bitrateBps float64, sustainedSec int) error
}
