package webrtc

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/test"
)

func testDegradeServer(observationSec int) *Server {
	return &Server{
		Parent:                test.NilLogger,
		DegradeInstantLossPct: 5,
		DegradeAvgLossPct:     1,
		DegradeObservationSec: observationSec,
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
	for i := 0; i < degradeAvgWindowSize; i++ {
		w.add(0, 100) // fully compliant, fills the window
	}
	require.Equal(t, 0.0, w.averageLossRate())

	// One more sample after the window is full should evict the oldest
	// (also-compliant) sample, not double-count or grow unbounded: window
	// now holds (degradeAvgWindowSize-1) samples of (0,100) plus this one
	// (100,0) sample. lost=100, total=100*(degradeAvgWindowSize-1)+100 =
	// 100*degradeAvgWindowSize.
	w.add(100, 0) // 100% loss, single sample
	avg := w.averageLossRate()
	require.InDelta(t, 100.0/(100.0*degradeAvgWindowSize), avg, 0.0001)
}

func TestDegradeStateSampleHandlesCounterReset(t *testing.T) {
	ds := newDegradeState("live/test", testDegradeServer(60))
	ds.observeSessionLayerCount(3)

	ds.sample(0, 0)  // first sample: baseline only, no delta
	ds.sample(5, 95) // cumulative: 5 lost, 95 received -> delta (5,95), fed into the window
	require.Equal(t, uint64(5), ds.lastLost)
	require.Equal(t, uint64(95), ds.lastReceived)
	avgBeforeReset := ds.window.averageLossRate()

	// Simulate a new WHIP session after a restart: cumulative counters
	// restart near zero (well below the previous lastLost/lastReceived).
	// Must be treated as a fresh baseline - re-basing lastLost/lastReceived
	// without computing a delta - not as a huge wrapped uint64 underflow
	// fed into the window.
	ds.sample(1, 2)
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
	srv := testDegradeServer(observationSec)
	ds := newDegradeState("live/test", srv)
	ds.observeSessionLayerCount(3)

	wait := func() { time.Sleep((observationSec*1000 + 200) * time.Millisecond) }

	sendCompliant := func() { ds.recordSample(0, 100) }
	sendNonCompliant := func() { ds.recordSample(50, 50) } // 50% instant loss, well over both thresholds

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
	// (degradeAvgWindowSize samples) that still holds the non-compliant
	// samples just sent above, so its average would stay well over
	// DegradeAvgLossPct for a long time (needing hundreds more compliant
	// samples to dilute) even though the *instant* rate is now perfectly
	// compliant. That "recovery takes time to heal the average" behavior
	// is real and intentional, but it's a property of lossWindow itself
	// (covered by TestLossWindowAverageLossRate/Wraps...), not something
	// this test is trying to verify - this test is about FSM step order.
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
	ds := newDegradeState("live/test", testDegradeServer(observationSec))
	ds.observeSessionLayerCount(3)

	ds.recordSample(50, 50) // non-compliant, starts the "bad" timer
	require.False(t, ds.badSince.IsZero())

	time.Sleep(400 * time.Millisecond) // well under the 1s window
	// Reset the average window before the compliant sample: otherwise the
	// single bad sample above would keep the 5-minute average over
	// threshold for a long time regardless of this one good sample,
	// which isn't what this test is checking (see the comment in
	// TestDegradeFullCycle for the same reasoning).
	ds.window = lossWindow{}
	ds.recordSample(0, 100) // compliant again - must reset, not degrade
	require.True(t, ds.badSince.IsZero())
	require.Equal(t, 3, ds.layers, "a brief compliant blip before the window elapses must not have been ignored")

	time.Sleep(1200 * time.Millisecond)
	ds.recordSample(0, 100) // still compliant, but no pending degrade to worry about
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
	ds := newDegradeState("live/test", testDegradeServer(observationSec))
	ds.observeSessionLayerCount(3)

	// First, short-lived session: a few non-compliant samples, then it
	// dies (nothing calls recordSample again for a while).
	ds.recordSample(50, 50)
	ds.recordSample(50, 50)
	require.False(t, ds.badSince.IsZero())
	require.Equal(t, 3, ds.layers, "6s of badness is nowhere near the 60s window yet")

	// Simulate badSince having been set a long time ago (minutes), as if
	// this test had actually waited that long, without needing the test
	// itself to sleep for real minutes.
	ds.badSince = time.Now().Add(-10 * time.Minute)
	ds.lastSampleAt = ds.badSince

	// New session establishes after the silent gap and immediately
	// samples non-compliant again.
	ds.recordSample(50, 50)
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
	ds := newDegradeState("live/test", testDegradeServer(60))
	require.Equal(t, 0, ds.maxLayers, "unknown until a session reports in")

	ds.observeSessionLayerCount(4)
	require.Equal(t, 4, ds.maxLayers)
	require.Equal(t, 4, ds.layers, "a fresh path starts fully up, not mid-degrade")
	require.Equal(t, 100, ds.bitratePercent)
}

// TestObserveSessionLayerCountGrowsCeilingOnCapacityIncrease covers
// switching the OBS profile up (e.g. 720p/3-layer -> 1080p/4-layer): the
// new, higher real count becomes the new ceiling, and is treated as a
// fresh full/undegraded starting point.
func TestObserveSessionLayerCountGrowsCeilingOnCapacityIncrease(t *testing.T) {
	ds := newDegradeState("live/test", testDegradeServer(60))
	ds.observeSessionLayerCount(3)
	ds.layers = 1 // pretend a degrade had happened
	ds.bitratePercent = 80

	ds.observeSessionLayerCount(4)
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
	ds := newDegradeState("live/test", testDegradeServer(60))
	ds.observeSessionLayerCount(4)
	ds.layers = 2 // FSM decided to degrade from 4 to 2

	// Executor restarts OBS with 2 real layers, as instructed.
	ds.observeSessionLayerCount(2)
	require.Equal(t, 4, ds.maxLayers, "ceiling must not shrink just because a session reconnected with fewer real layers")
	require.Equal(t, 2, ds.layers, "the FSM's own degrade decision must not be clobbered by observing the session it caused")
}

func TestDegradeDoesNothingBeforeMaxLayersIsKnown(t *testing.T) {
	const observationSec = 1
	ds := newDegradeState("live/test", testDegradeServer(observationSec))
	// No observeSessionLayerCount call - maxLayers is still 0.

	ds.recordSample(50, 50)
	time.Sleep(1200 * time.Millisecond)
	ds.recordSample(50, 50)
	require.Equal(t, 0, ds.layers, "nothing to degrade without a known ceiling")
	require.Equal(t, 100, ds.bitratePercent)
}
