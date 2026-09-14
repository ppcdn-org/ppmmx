package degrade

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/test"
)

// testThresholds sets Recover* equal to Degrade*, collapsing the hysteresis
// dead zone to zero width - every sample is either good or bad, exactly
// reproducing the pre-hysteresis single-threshold behavior the other tests
// in this file were written against. See TestDegradeHysteresisDeadZoneFreezesTimer
// for the dead zone itself.
func testThresholds(observationSec int) Thresholds {
	return Thresholds{
		DegradeInstantLossPct: 5,
		DegradeAvgLossPct:     1,
		RecoverInstantLossPct: 5,
		RecoverAvgLossPct:     1,
		ObservationSec:        observationSec,
	}
}

func TestLossWindowAverageLossRate(t *testing.T) {
	var w lossWindow
	require.Equal(t, 0.0, w.averageLossRate(), "empty window has no loss")

	w.add(1, 9) // 10% this sample
	w.add(0, 10)
	require.InDelta(t, 1.0/20.0, w.averageLossRate(), 0.0001)
}

func TestLossWindowWrapsAfterFilling(t *testing.T) {
	var w lossWindow
	for i := 0; i < AvgWindowSize; i++ {
		w.add(0, 100) // fully compliant, fills the window
	}
	require.Equal(t, 0.0, w.averageLossRate())

	// One more sample after the window is full should evict the oldest
	// (also-compliant) sample, not double-count or grow unbounded: window
	// now holds (AvgWindowSize-1) samples of (0,100) plus this one
	// (100,0) sample. lost=100, total=100*(AvgWindowSize-1)+100 =
	// 100*AvgWindowSize.
	w.add(100, 0) // 100% loss, single sample
	avg := w.averageLossRate()
	require.InDelta(t, 100.0/(100.0*AvgWindowSize), avg, 0.0001)
}

func TestDegradeStateSampleHandlesCounterReset(t *testing.T) {
	ds := newState("live/test", test.NilLogger)
	ds.ObserveSessionLayerCount(3)
	th := testThresholds(60)

	ds.Sample(0, 0, th)  // first sample: baseline only, no delta
	ds.Sample(5, 95, th) // cumulative: 5 lost, 95 received -> delta (5,95), fed into the window
	require.Equal(t, uint64(5), ds.lastLost)
	require.Equal(t, uint64(95), ds.lastReceived)
	avgBeforeReset := ds.window.averageLossRate()

	// Simulate a new WHIP session after a restart: cumulative counters
	// restart near zero (well below the previous lastLost/lastReceived).
	// Must be treated as a fresh baseline - re-basing lastLost/lastReceived
	// without computing a delta - not as a huge wrapped uint64 underflow
	// fed into the window.
	ds.Sample(1, 2, th)
	require.Equal(t, uint64(1), ds.lastLost)
	require.Equal(t, uint64(2), ds.lastReceived)
	require.Equal(t, avgBeforeReset, ds.window.averageLossRate(),
		"a counter reset must not add anything to the window (no delta computed for it)")
}

// TestDegradeFullCycle drives the FSM through every degrade step, the
// terminal alert, and every recovery step, using a short observation
// window so the test runs in real time without mocking the clock.
func TestDegradeFullCycle(t *testing.T) {
	const observationSec = 1
	th := testThresholds(observationSec)
	ds := newState("live/test", test.NilLogger)
	ds.ObserveSessionLayerCount(3)

	wait := func() { time.Sleep((observationSec*1000 + 200) * time.Millisecond) }

	sendCompliant := func() { ds.recordSample(0, 100, th) }
	sendNonCompliant := func() { ds.recordSample(50, 50, th) } // 50% instant loss, well over both thresholds

	require.Equal(t, 3, ds.layers)
	require.Equal(t, 100, ds.bitratePercent)

	// Degrade: 3 -> 2 -> 1 -> bitrate 80% -> terminal (stays at 1/80%).
	sendNonCompliant()
	wait()
	sendNonCompliant()
	require.Equal(t, 2, ds.layers, "first sustained non-compliance should drop one layer")

	sendNonCompliant()
	wait()
	sendNonCompliant()
	require.Equal(t, 1, ds.layers)
	require.Equal(t, 100, ds.bitratePercent)

	sendNonCompliant()
	wait()
	sendNonCompliant()
	require.Equal(t, 1, ds.layers)
	require.Equal(t, 80, ds.bitratePercent, "layer floor reached, should now degrade bitrate")

	// Terminal: still non-compliant at layers=1/bitrate=80% - no further
	// automatic action, state must not change.
	sendNonCompliant()
	wait()
	sendNonCompliant()
	require.Equal(t, 1, ds.layers)
	require.Equal(t, 80, ds.bitratePercent, "terminal state must not degrade further")

	// Recovery: bitrate first (LIFO), then layers back up 1 -> 2 -> 3.
	//
	// Reset the 5-minute average window here: it's a bounded ring buffer
	// (AvgWindowSize samples) that still holds the non-compliant samples
	// just sent above, so its average would stay well over AvgLossPct for
	// a long time (needing hundreds more compliant samples to dilute) even
	// though the *instant* rate is now perfectly compliant. That "recovery
	// takes time to heal the average" behavior is real and intentional,
	// but it's a property of lossWindow itself (covered by
	// TestLossWindowAverageLossRate/Wraps...), not something this test is
	// trying to verify - this test is about FSM step order.
	ds.window = lossWindow{}
	sendCompliant()
	wait()
	sendCompliant()
	require.Equal(t, 100, ds.bitratePercent, "compliance should restore bitrate before layers")
	require.Equal(t, 1, ds.layers)

	sendCompliant()
	wait()
	sendCompliant()
	require.Equal(t, 2, ds.layers)

	sendCompliant()
	wait()
	sendCompliant()
	require.Equal(t, 3, ds.layers)
	require.Equal(t, 100, ds.bitratePercent)

	// Fully recovered: further compliance must not do anything else.
	sendCompliant()
	wait()
	sendCompliant()
	require.Equal(t, 3, ds.layers)
	require.Equal(t, 100, ds.bitratePercent)
}

func TestDegradeResetsOnFlipBeforeObservationWindowElapses(t *testing.T) {
	const observationSec = 1
	th := testThresholds(observationSec)
	ds := newState("live/test", test.NilLogger)
	ds.ObserveSessionLayerCount(3)

	ds.recordSample(50, 50, th) // non-compliant, starts the "bad" timer
	require.False(t, ds.badSince.IsZero())

	time.Sleep(400 * time.Millisecond) // well under the 1s window
	// Reset the average window before the compliant sample: otherwise the
	// single bad sample above would keep the 5-minute average over
	// threshold for a long time regardless of this one good sample,
	// which isn't what this test is checking (see the comment in
	// TestDegradeFullCycle for the same reasoning).
	ds.window = lossWindow{}
	ds.recordSample(0, 100, th) // compliant again - must reset, not degrade
	require.True(t, ds.badSince.IsZero())
	require.Equal(t, 3, ds.layers, "a brief compliant blip before the window elapses must not have been ignored")

	time.Sleep(1200 * time.Millisecond)
	ds.recordSample(0, 100, th) // still compliant, but no pending degrade to worry about
	require.Equal(t, 3, ds.layers)
}

// TestDegradeLongSilenceDoesNotInheritStaleBadSince is a regression test
// for a real bug found during remote integration testing: a brief,
// non-compliant WHIP session (a few seconds, well under the observation
// window) followed by a long gap with no active session at all (OBS
// disconnected, reconnect attempts failing) - then a new session finally
// establishes and immediately samples non-compliant again. Before this
// fix, badSince was set once during the first brief session and never
// touched during the silent gap (nothing was sampling), so the very next
// sample after the gap saw "now - badSince" already far exceeding the
// observation window and degraded instantly, crediting minutes of total
// silence as "continuously observed non-compliance".
func TestDegradeLongSilenceDoesNotInheritStaleBadSince(t *testing.T) {
	const observationSec = 60 // the real default, not shortened - see below
	th := testThresholds(observationSec)
	ds := newState("live/test", test.NilLogger)
	ds.ObserveSessionLayerCount(3)

	// First, short-lived session: a few non-compliant samples, then it
	// dies (nothing calls recordSample again for a while).
	ds.recordSample(50, 50, th)
	ds.recordSample(50, 50, th)
	require.False(t, ds.badSince.IsZero())
	require.Equal(t, 3, ds.layers, "6s of badness is nowhere near the 60s window yet")

	// Simulate badSince having been set a long time ago (minutes), as if
	// this test had actually waited that long, without needing the test
	// itself to sleep for real minutes.
	ds.badSince = time.Now().Add(-10 * time.Minute)
	ds.lastSampleAt = ds.badSince

	// New session establishes after the silent gap and immediately
	// samples non-compliant again.
	ds.recordSample(50, 50, th)
	require.Equal(t, 3, ds.layers,
		"a stale badSince from before a long silent gap must not be treated as 10 minutes of continuous non-compliance")
	require.False(t, ds.badSince.IsZero(), "the new sample is still non-compliant, so a fresh bad timer should now be running")
	require.WithinDuration(t, time.Now(), ds.badSince, 2*time.Second, "badSince must have been rebased to now, not left at the stale value")
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
	ds.bitratePercent = 80

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
	const observationSec = 1
	th := testThresholds(observationSec)
	ds := newState("live/test", test.NilLogger)
	// No ObserveSessionLayerCount call - maxLayers is still 0.

	ds.recordSample(50, 50, th)
	time.Sleep(1200 * time.Millisecond)
	ds.recordSample(50, 50, th)
	require.Equal(t, 0, ds.layers, "nothing to degrade without a known ceiling")
	require.Equal(t, 100, ds.bitratePercent)
}

// TestDegradeHysteresisDeadZoneFreezesTimer covers the Degrade*/Recover*
// split (hysteresis): a loss rate strictly between Recover* and Degrade* is
// a dead zone that does nothing at all - it neither cancels/rebases an
// already-running badSince/goodSince timer, nor checks/fires one itself
// (only a sample that is itself decisively good or bad does that). A timer
// started by a genuinely bad sample must survive being followed by dead-
// zone samples untouched, so a *later* bad sample correctly sees the full
// elapsed time since the original one and fires - but the dead-zone
// samples in between must never themselves trigger the step. A lone dead-
// zone sample with no prior timer running must not start one either.
func TestDegradeHysteresisDeadZoneFreezesTimer(t *testing.T) {
	const observationSec = 1
	th := Thresholds{
		DegradeInstantLossPct: 10, // >10% is bad
		DegradeAvgLossPct:     10,
		RecoverInstantLossPct: 2, // <=2% is good
		RecoverAvgLossPct:     2,
		ObservationSec:        observationSec,
	}
	ds := newState("live/test", test.NilLogger)
	ds.ObserveSessionLayerCount(3)

	// window reset before each sample below isolates instant loss as the
	// only signal in play (avg == instant for a lone sample in a fresh
	// window), same technique the flip/reset tests above already use.
	ds.window = lossWindow{}
	ds.recordSample(20, 80, th) // 20% > 10% degrade threshold: bad, starts badSince
	require.False(t, ds.badSince.IsZero())
	firstBadSince := ds.badSince

	time.Sleep(400 * time.Millisecond) // well under the 1s window

	ds.window = lossWindow{}
	ds.recordSample(5, 95, th) // 5%: dead zone (between 2% recover and 10% degrade)
	require.False(t, ds.badSince.IsZero(), "a dead-zone sample must not clear badSince")
	require.Equal(t, firstBadSince, ds.badSince, "a dead-zone sample must not rebase badSince either")
	require.Equal(t, 3, ds.layers, "a dead-zone sample must never itself trigger a degrade step")

	time.Sleep(700 * time.Millisecond) // ~1.1s elapsed since firstBadSince now

	ds.window = lossWindow{}
	ds.recordSample(20, 80, th) // bad again: badSince (still firstBadSince) is now old enough to fire
	require.Equal(t, 2, ds.layers,
		"badSince must have survived the dead-zone sample untouched, so a later bad sample "+
			"correctly sees the full elapsed time since the ORIGINAL bad sample and fires")

	// A dead-zone sample with no timer already running must not start one.
	ds2 := newState("live/test2", test.NilLogger)
	ds2.ObserveSessionLayerCount(3)
	ds2.window = lossWindow{}
	ds2.recordSample(5, 95, th)
	require.True(t, ds2.badSince.IsZero(), "a dead-zone sample alone must not start a bad timer")
	require.True(t, ds2.goodSince.IsZero(), "a dead-zone sample alone must not start a good timer either")
	require.Equal(t, 3, ds2.layers)
}
