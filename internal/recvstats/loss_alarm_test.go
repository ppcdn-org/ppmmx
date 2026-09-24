package recvstats

import (
	"testing"
	"time"
)

var epoch = time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

func TestSustainedLossTrackerStaysQuietWhenNeverExceeded(t *testing.T) {
	var tr SustainedLossTracker
	for i := 0; i < 3; i++ {
		d := tr.Update(5, 10, true, 120*time.Second, epoch.Add(time.Duration(i)*60*time.Second))
		if d.ShouldReport || d.ShouldDisconnect {
			t.Fatalf("sample %d: loss under threshold must never report or disconnect, got %+v", i, d)
		}
	}
}

func TestSustainedLossTrackerExactlyAtThresholdDoesNotCount(t *testing.T) {
	var tr SustainedLossTracker
	d := tr.Update(10, 10, true, 120*time.Second, epoch)
	if d.ShouldReport || d.ShouldDisconnect {
		t.Fatalf("loss exactly at threshold must not count as exceeding it, got %+v", d)
	}
}

func TestSustainedLossTrackerSingleSpikeFiresOneResolve(t *testing.T) {
	var tr SustainedLossTracker

	d := tr.Update(15, 10, true, 120*time.Second, epoch)
	if !d.ShouldReport || d.Sustained != 0 {
		t.Fatalf("first over-threshold sample must report with zero sustained duration, got %+v", d)
	}

	d = tr.Update(5, 10, true, 120*time.Second, epoch.Add(60*time.Second))
	if !d.ShouldReport || d.ShouldDisconnect {
		t.Fatalf("drop back under threshold must fire exactly one resolve report, got %+v", d)
	}

	d = tr.Update(5, 10, true, 120*time.Second, epoch.Add(120*time.Second))
	if d.ShouldReport {
		t.Fatalf("staying under threshold after the resolve must not report again, got %+v", d)
	}
}

func TestSustainedLossTrackerDisconnectsAfterThreeConsecutiveSamples(t *testing.T) {
	var tr SustainedLossTracker

	d := tr.Update(15, 10, true, 120*time.Second, epoch) // t=0
	if d.ShouldDisconnect {
		t.Fatalf("t=0: must not disconnect yet, got %+v", d)
	}

	d = tr.Update(15, 10, true, 120*time.Second, epoch.Add(60*time.Second)) // t=60
	if d.ShouldDisconnect {
		t.Fatalf("t=60: must not disconnect yet (only 60s sustained), got %+v", d)
	}

	d = tr.Update(15, 10, true, 120*time.Second, epoch.Add(120*time.Second)) // t=120
	if !d.ShouldDisconnect {
		t.Fatalf("t=120: must disconnect once sustained duration reaches the threshold, got %+v", d)
	}
	if d.Sustained != 120*time.Second {
		t.Fatalf("expected Sustained=120s at t=120, got %v", d.Sustained)
	}
}

func TestSustainedLossTrackerDisconnectDisabledNeverFires(t *testing.T) {
	var tr SustainedLossTracker
	for i := 0; i <= 3; i++ {
		d := tr.Update(15, 10, false, 120*time.Second, epoch.Add(time.Duration(i)*60*time.Second))
		if d.ShouldDisconnect {
			t.Fatalf("sample %d: disconnect must never fire when disabled, got %+v", i, d)
		}
	}
}

// The loss-recycle tier (see conf.{RTP,SRT}LossRecycle*) drives the same
// tracker with a low threshold and a long (1h) window at the 60s sample
// cadence: mild loss (1.5% over a 1.0% threshold) must not recycle until it
// has stayed over for a full hour of consecutive samples, and one clean
// minute inside the hour resets the clock (it is a CHRONIC-loss signal, not
// a cumulative-minutes one).
func TestSustainedLossTrackerMildLossRecyclesOnlyAfterAnHour(t *testing.T) {
	var tr SustainedLossTracker
	const threshold, window = 1.0, time.Hour

	// 59 consecutive over-threshold minutes: sustained climbs but no recycle
	// yet (t=0 is the first over-threshold sample, sustained still 0).
	for i := 0; i < 60; i++ {
		d := tr.Update(1.5, threshold, true, window, epoch.Add(time.Duration(i)*60*time.Second))
		if d.ShouldDisconnect {
			t.Fatalf("minute %d (%.0fs sustained): must not recycle before a full hour", i, d.Sustained.Seconds())
		}
	}

	// t=3600s: exactly one hour continuously over threshold -> recycle.
	d := tr.Update(1.5, threshold, true, window, epoch.Add(3600*time.Second))
	if !d.ShouldDisconnect {
		t.Fatalf("t=3600s: mild loss sustained a full hour must recycle, got %+v", d)
	}
	if d.Sustained != window {
		t.Fatalf("expected Sustained=1h at t=3600s, got %v", d.Sustained)
	}
}

// One clean minute in the middle of the hour must reset the recycle clock:
// the tier fires only on genuinely continuous loss.
func TestSustainedLossTrackerMildLossOneCleanMinuteResetsRecycle(t *testing.T) {
	var tr SustainedLossTracker
	const threshold, window = 1.0, time.Hour

	for i := 0; i < 59; i++ { // 0..58 over threshold
		tr.Update(1.5, threshold, true, window, epoch.Add(time.Duration(i)*60*time.Second))
	}
	tr.Update(0.2, threshold, true, window, epoch.Add(59*60*time.Second)) // clean minute resets
	d := tr.Update(1.5, threshold, true, window, epoch.Add(60*60*time.Second))
	if d.Sustained != 0 {
		t.Fatalf("a fresh over-threshold spell after one clean minute must restart the clock at zero, got %v", d.Sustained)
	}
	if d.ShouldDisconnect {
		t.Fatalf("must not recycle right after the clock reset, got %+v", d)
	}
}

// A brief recovery must reset the sustained-duration clock rather than
// letting two separate over-threshold spells accumulate together - the
// requirement is 120 CONTINUOUS seconds, not 120s total.
func TestSustainedLossTrackerRecoveryResetsSustainedClock(t *testing.T) {
	var tr SustainedLossTracker

	tr.Update(15, 10, true, 120*time.Second, epoch)                           // t=0, over
	tr.Update(15, 10, true, 120*time.Second, epoch.Add(60*time.Second))       // t=60, over (60s sustained)
	tr.Update(5, 10, true, 120*time.Second, epoch.Add(120*time.Second))       // t=120, recovers - resolve fires, clock resets
	d := tr.Update(15, 10, true, 120*time.Second, epoch.Add(180*time.Second)) // t=180, over again - fresh start
	if d.Sustained != 0 {
		t.Fatalf("a fresh over-threshold spell after recovery must start its sustained clock at zero, got %v", d.Sustained)
	}
	if d.ShouldDisconnect {
		t.Fatalf("must not disconnect immediately after a reset, got %+v", d)
	}
}
