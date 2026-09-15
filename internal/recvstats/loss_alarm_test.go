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
