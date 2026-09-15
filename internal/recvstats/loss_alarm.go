package recvstats

import "time"

// SustainedLossTracker turns successive Snapshot.LossPct samples (one every
// Interval) into a raise/refresh/resolve decision plus a continuous-over-
// threshold duration, without knowing anything about sockets, HTTP, or which
// ingest protocol fed it - shared by every protocol server that reports a
// loss-rate alarm to ppcenter (SRT, WHIP, ...). Not safe for concurrent use -
// one tracker per ingest connection/session, driven only from that
// connection's own stats goroutine.
type SustainedLossTracker struct {
	exceededSince time.Time
	wasOver       bool
}

// LossAlarmDecision is what to do with one sample.
type LossAlarmDecision struct {
	// ShouldReport is true on every sample while loss stays over threshold
	// (so ppcenter's alarm stays fresh/active), and exactly once more on the
	// sample where it drops back under threshold (the resolve report).
	ShouldReport bool
	// Sustained is how long loss has been continuously over threshold as of
	// this sample; zero when not currently over.
	Sustained time.Duration
	// ShouldDisconnect is true once Sustained has reached disconnectAfter,
	// for as long as the caller keeps calling Update (the caller is
	// expected to act on the first true and stop calling, since the
	// connection is being torn down). Callers that don't offer a disconnect
	// feature (e.g. WHIP) simply pass disconnectEnable=false and ignore it.
	ShouldDisconnect bool
}

// Update folds in one new sample. thresholdPct and disconnectAfter are
// re-read from config on every call rather than captured once, so a
// mid-connection config reload takes effect on the next sample.
func (t *SustainedLossTracker) Update(lossPct, thresholdPct float64, disconnectEnable bool, disconnectAfter time.Duration, now time.Time) LossAlarmDecision {
	over := lossPct > thresholdPct
	if !over {
		wasOver := t.wasOver
		t.exceededSince, t.wasOver = time.Time{}, false
		return LossAlarmDecision{ShouldReport: wasOver}
	}

	if t.exceededSince.IsZero() {
		t.exceededSince = now
	}
	t.wasOver = true
	sustained := now.Sub(t.exceededSince)

	return LossAlarmDecision{
		ShouldReport:     true,
		Sustained:        sustained,
		ShouldDisconnect: disconnectEnable && sustained >= disconnectAfter,
	}
}
