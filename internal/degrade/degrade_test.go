package degrade

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/test"
)

// testThresholds builds a Thresholds with a hysteresis band, used across the
// FSM tests. RaisePct=5, LowerPct=1; a 50% sample is decisively bad and a 0%
// sample decisively good.
func testThresholds(observationSec int) Thresholds {
	return Thresholds{
		RaisePct:       5,
		LowerPct:       1,
		ObservationSec: observationSec,
	}
}

func TestDegradeStateSampleHandlesCounterReset(t *testing.T) {
	ds := newState("live/test", test.NilLogger)
	ds.ObserveSessionLayerCount(3)
	th := testThresholds(60)

	ds.Sample(0, 0, th)   // first sample: baseline only, no delta
	ds.Sample(5, 100, th) // cumulative: 5 unrecoverable, 100 total -> delta (5, 100)
	require.Equal(t, uint64(5), ds.lastUnrecov)
	require.Equal(t, uint64(100), ds.lastTotal)

	// Simulate a new session: cumulative counters restart near zero, below
	// the previous ones. Must be treated as a fresh baseline, not a huge
	// wrapped uint64 underflow.
	ds.Sample(1, 2, th)
	require.Equal(t, uint64(1), ds.lastUnrecov)
	require.Equal(t, uint64(2), ds.lastTotal)
}

// TestDegradeFullCycle drives the FSM through the bitrate phase, the layer
// phase, the terminal alert, and every recovery step, using a short
// observation window so the test runs in real time without mocking the clock.
func TestDegradeFullCycle(t *testing.T) {
	const observationSec = 1
	th := testThresholds(observationSec)
	ds := newState("live/test", test.NilLogger)
	ds.ObserveSessionLayerCount(3)

	wait := func() { time.Sleep(time.Duration(observationSec)*time.Second + 200*time.Millisecond) }

	// 50% ULR: decisively bad. 100% total, 0 unrecoverable.
	sendBad := func() { ds.recordSample(50, 100, th) }
	sendGood := func() { ds.recordSample(0, 100, th) }

	require.Equal(t, 3, ds.layers)
	require.Equal(t, 100, ds.bitratePercent)

	// Bitrate phase: 100% -> 80% -> 60%, immediate on each first bad sample
	// after the cooldown.
	sendBad()
	require.Equal(t, 80, ds.bitratePercent, "first bad sample degrades bitrate immediately")
	require.Equal(t, 3, ds.layers)

	wait()
	sendBad()
	require.Equal(t, 60, ds.bitratePercent, "second bad sample drops bitrate to the floor")
	require.Equal(t, 3, ds.layers)

	// Layer phase: 3 -> 2 -> 1, only after bitrate is at the floor.
	wait()
	sendBad()
	require.Equal(t, 2, ds.layers, "bitrate at floor, now drop a layer")
	require.Equal(t, 60, ds.bitratePercent)

	wait()
	sendBad()
	require.Equal(t, 1, ds.layers)
	require.Equal(t, 60, ds.bitratePercent)

	// Terminal: layers=1, bitrate=60%, still non-compliant - no further
	// automatic action, state must not change.
	wait()
	sendBad()
	require.Equal(t, 1, ds.layers)
	require.Equal(t, 60, ds.bitratePercent, "terminal state must not degrade further")

	// Recovery: layers first (1 -> 2 -> 3), then bitrate (60 -> 80 -> 100).
	// Each step needs sustained compliance for observationSec.
	recoverStep := func() {
		sendGood() // starts/keeps the good timer
		wait()
		sendGood() // goodSince is now old enough to fire
	}

	recoverStep()
	require.Equal(t, 2, ds.layers, "recovery adds a layer before restoring bitrate")
	require.Equal(t, 60, ds.bitratePercent)

	recoverStep()
	require.Equal(t, 3, ds.layers)
	require.Equal(t, 60, ds.bitratePercent)

	recoverStep()
	require.Equal(t, 100-20, ds.bitratePercent, "layers restored, now bitrate")
	require.Equal(t, 3, ds.layers)

	recoverStep()
	require.Equal(t, 100, ds.bitratePercent)
	require.Equal(t, 3, ds.layers)

	// Fully recovered: further compliance must not do anything else.
	recoverStep()
	require.Equal(t, 3, ds.layers)
	require.Equal(t, 100, ds.bitratePercent)
}

// TestDegradeImmediateThenCooldown verifies the degrade trigger fires on the
// first threshold-crossing sample, then a cooldown prevents another step
// until ObservationSec has elapsed.
func TestDegradeImmediateThenCooldown(t *testing.T) {
	th := testThresholds(10) // long cooldown so the test can observe it
	ds := newState("live/test", test.NilLogger)
	ds.ObserveSessionLayerCount(3)

	ds.recordSample(50, 100, th)
	require.Equal(t, 80, ds.bitratePercent, "first bad sample fires immediately")

	// More bad samples inside the cooldown must not advance the ladder.
	ds.recordSample(50, 100, th)
	ds.recordSample(50, 100, th)
	require.Equal(t, 80, ds.bitratePercent)
	require.Equal(t, 3, ds.layers)
}

// TestRecoverRequiresSustainedCompliance verifies a single compliant sample
// does not recover; compliance must persist for ObservationSec.
func TestRecoverRequiresSustainedCompliance(t *testing.T) {
	const observationSec = 1
	th := testThresholds(observationSec)
	ds := newState("live/test", test.NilLogger)
	ds.ObserveSessionLayerCount(3)

	// Get to a degraded state: one bad sample -> bitrate 80.
	ds.recordSample(50, 100, th)
	require.Equal(t, 80, ds.bitratePercent)

	// A lone good sample must not immediately recover.
	ds.recordSample(0, 100, th)
	require.Equal(t, 80, ds.bitratePercent, "one good sample is not enough")

	// After the observation window, another good sample recovers.
	time.Sleep(time.Duration(observationSec)*time.Second + 200*time.Millisecond)
	ds.recordSample(0, 100, th)
	require.Equal(t, 100, ds.bitratePercent)
}

// TestDegradeDeadZoneDoesNotAdvance verifies a ULR strictly between LowerPct
// and RaisePct neither degrades nor recovers.
func TestDegradeDeadZoneDoesNotAdvance(t *testing.T) {
	th := Thresholds{RaisePct: 10, LowerPct: 2, ObservationSec: 1}
	ds := newState("live/test", test.NilLogger)
	ds.ObserveSessionLayerCount(3)

	// 5% is inside the dead zone (2 < 5 <= 10).
	ds.recordSample(5, 100, th)
	require.Equal(t, 3, ds.layers)
	require.Equal(t, 100, ds.bitratePercent)

	// Dead zone must not have started a recovery timer either.
	require.True(t, ds.goodSince.IsZero())
}

// TestDegradeLongSilenceResetsTimers is a regression test for the stale
// observation-epoch bug: a brief non-compliant burst followed by a long
// silent gap (no active publish) must not be credited as continuous
// non-compliance when sampling resumes.
func TestDegradeLongSilenceResetsTimers(t *testing.T) {
	th := testThresholds(60)
	ds := newState("live/test", test.NilLogger)
	ds.ObserveSessionLayerCount(3)

	// Simulate a recovery timer having been started a long time ago, then a
	// silent gap.
	ds.goodSince = time.Now().Add(-10 * time.Minute)
	ds.lastSampleAt = ds.goodSince

	// A compliant sample after the gap must treat it as a fresh epoch: it
	// starts a new good timer at now rather than firing a recovery on the
	// stale timestamp. The FSM is already fully up, so assert the timer was
	// rebased.
	ds.recordSample(0, 100, th)
	require.WithinDuration(t, time.Now(), ds.goodSince, 2*time.Second,
		"goodSince must be rebased after a long silent gap")
}

// TestObserveSessionLayerCountInitializesFromFirstSession is a regression
// test for a real gap found during remote testing: the FSM used to
// hardcode maxLayers=3, but different OBS profiles negotiate different
// real layer counts (e.g. 4 for 1080p, 3 for 720p) - a fresh path must
// pick up whatever the first real session actually reports.
func TestObserveSessionLayerCountInitializesFromFirstSession(t *testing.T) {
	ds := newState("live/test", test.NilLogger)
	require.Equal(t, 0, ds.maxLayers, "unknown until a session reports in")

	ds.ObserveSessionLayerCount(4)
	require.Equal(t, 4, ds.maxLayers)
	require.Equal(t, 4, ds.layers, "a fresh path starts fully up, not mid-degrade")
	require.Equal(t, 100, ds.bitratePercent)
}

// TestObserveSessionLayerCountGrowsCeilingOnCapacityIncrease covers
// switching the OBS profile up (e.g. 720p/3-layer -> 1080p/4-layer): the
// new, higher real count becomes the new ceiling, and is treated as a
// fresh full/undegraded starting point.
func TestObserveSessionLayerCountGrowsCeilingOnCapacityIncrease(t *testing.T) {
	ds := newState("live/test", test.NilLogger)
	ds.ObserveSessionLayerCount(3)
	ds.layers = 1 // pretend a degrade had happened
	ds.bitratePercent = 60

	ds.ObserveSessionLayerCount(4)
	require.Equal(t, 4, ds.maxLayers)
	require.Equal(t, 4, ds.layers, "a capacity increase resets to the new full state")
	require.Equal(t, 100, ds.bitratePercent)
}

// TestObserveSessionLayerCountDoesNotShrinkCeilingOnDegradeRestart is the
// core reason maxLayers exists as a separate field from layers: an
// executor carrying out a degrade instruction reconnects with fewer real
// layers than the ceiling (that's the whole point of the degrade), and
// this must NOT be mistaken for the deployment's capacity shrinking -
// otherwise recovery could never climb back above whatever the last
// degrade-restart used.
func TestObserveSessionLayerCountDoesNotShrinkCeilingOnDegradeRestart(t *testing.T) {
	ds := newState("live/test", test.NilLogger)
	ds.ObserveSessionLayerCount(4)
	ds.layers = 2 // FSM decided to degrade from 4 to 2

	// Executor restarts OBS with 2 real layers, as instructed.
	ds.ObserveSessionLayerCount(2)
	require.Equal(t, 4, ds.maxLayers, "ceiling must not shrink just because a session reconnected with fewer real layers")
	require.Equal(t, 2, ds.layers, "the FSM's own degrade decision must not be clobbered by observing the session it caused")
}

func TestDegradeDoesNothingBeforeMaxLayersIsKnown(t *testing.T) {
	th := testThresholds(1)
	ds := newState("live/test", test.NilLogger)
	// No ObserveSessionLayerCount call - maxLayers is still 0.

	ds.recordSample(50, 100, th)
	require.Equal(t, 0, ds.layers, "nothing to degrade without a known ceiling")
	require.Equal(t, 100, ds.bitratePercent)
}

// TestActionReturned verifies recordSample reports the action it fired, so
// callers (e.g. SRT) can adjust latency accordingly.
func TestActionReturned(t *testing.T) {
	th := testThresholds(1)
	ds := newState("live/test", test.NilLogger)
	ds.ObserveSessionLayerCount(3)

	require.Equal(t, ActionDegrade, ds.recordSample(50, 100, th), "bad sample degrades")
	require.Equal(t, ActionNone, ds.recordSample(50, 100, th), "cooldown suppresses the next action")

	// Clear the cooldown and drop to the bitrate floor to reach the layer
	// phase: a layer cut is reported as ActionRestart, distinct from the
	// bitrate-only ActionDegrade above.
	ds.degradeCooldownUntil = time.Time{}
	ds.bitratePercent = 60
	require.Equal(t, ActionRestart, ds.recordSample(50, 100, th), "a layer cut reports a restart")

	ds.degradeCooldownUntil = time.Time{} // clear cooldown for the recovery check
	require.Equal(t, ActionNone, ds.recordSample(0, 100, th), "first good sample only starts the timer")

	time.Sleep(1200 * time.Millisecond)
	require.Equal(t, ActionRecover, ds.recordSample(0, 100, th), "sustained compliance recovers")
}

// TestDegradeLayerPhaseGatedByRestartPct verifies that a ULR between RaisePct
// and RestartPct only runs the non-disruptive bitrate phase: the ladder holds
// at its bitrate floor instead of cutting a layer (which would restart the
// publish). The layer phase only resumes once ULR exceeds RestartPct.
func TestDegradeLayerPhaseGatedByRestartPct(t *testing.T) {
	const observationSec = 1
	th := Thresholds{RaisePct: 5, LowerPct: 1, RestartPct: 10, ObservationSec: observationSec}
	ds := newState("live/test", test.NilLogger)
	ds.ObserveSessionLayerCount(3)

	wait := func() { time.Sleep(time.Duration(observationSec)*time.Second + 200*time.Millisecond) }

	// 7%: above RaisePct (degrade) but below RestartPct (bitrate only).
	require.Equal(t, ActionDegrade, ds.recordSample(7, 100, th))
	require.Equal(t, 80, ds.bitratePercent)
	require.Equal(t, 3, ds.layers)

	wait()
	require.Equal(t, ActionDegrade, ds.recordSample(7, 100, th))
	require.Equal(t, 60, ds.bitratePercent)
	require.Equal(t, 3, ds.layers)

	// At the bitrate floor and still below RestartPct: hold, no layer cut.
	wait()
	require.Equal(t, ActionDegrade, ds.recordSample(7, 100, th))
	require.Equal(t, 60, ds.bitratePercent, "bitrate is already at the floor")
	require.Equal(t, 3, ds.layers, "must not cut a layer below the restart threshold")

	// Above RestartPct: the layer phase runs and reports a restart.
	wait()
	require.Equal(t, ActionRestart, ds.recordSample(15, 100, th))
	require.Equal(t, 2, ds.layers)
}
