package webrtc

import (
	"testing"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/stretchr/testify/require"

	webrtcproto "github.com/bluenviron/mediamtx/internal/protocols/webrtc"
)

// Three video layers plus audio, matching layerDefaults in
// track_selector.go: track 0 = 2000kbps, 1 = 1000kbps, 2 = 400kbps. Sorted
// low-to-high that is the ladder 2 -> 1 -> 0.
func makeLadderSelector(t *testing.T) *webrtcproto.TrackSelector {
	t.Helper()

	desc := &description.Session{
		Medias: []*description.Media{
			{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PayloadTyp: 96, PacketizationMode: 1}}},
			{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PayloadTyp: 96, PacketizationMode: 1}}},
			{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PayloadTyp: 96, PacketizationMode: 1}}},
			{Type: description.MediaTypeAudio, Formats: []format.Format{&format.Opus{PayloadTyp: 111}}},
		},
	}

	sel := webrtcproto.NewTrackSelector(nil, func(_, _ int) {})
	// Passthrough so Select takes effect immediately: these tests are about
	// the decision, not about keyframe alignment (covered by
	// TestTrackSelectorSwitchOnKeyframe).
	sel.SetPassthrough(true)
	require.NoError(t, sel.LoadFromDescription(desc))
	return sel
}

func TestControllerUpgradesOneStepAfterConfirmations(t *testing.T) {
	sel := makeLadderSelector(t)
	require.NoError(t, sel.Select(2)) // start at the bottom
	c := newABRController(sel)

	// No loss, stable RTT: upgrade is warranted, but a single sample must
	// not move anything.
	for i := range abrUpgradeConfirmations - 1 {
		_, ok := c.evaluate(0, 30)
		require.Falsef(t, ok, "upgraded after only %d confirmations", i+1)
	}

	target, ok := c.evaluate(0, 30)
	require.True(t, ok)
	require.Equal(t, 1, target, "one step up from 2 is 1, not straight to the top")
}

func TestControllerDowngradesOneStepFaster(t *testing.T) {
	sel := makeLadderSelector(t)
	require.NoError(t, sel.Select(0)) // start at the top
	c := newABRController(sel)

	for i := range abrDowngradeConfirmations - 1 {
		_, ok := c.evaluate(10, 30)
		require.Falsef(t, ok, "downgraded after only %d confirmations", i+1)
	}

	target, ok := c.evaluate(10, 30)
	require.True(t, ok)
	require.Equal(t, 1, target, "one step down from 0 is 1, not straight to the bottom")

	// Downgrades are meant to react faster than upgrades.
	require.Less(t, abrDowngradeConfirmations, abrUpgradeConfirmations)
}

// Loss between the upgrade and downgrade thresholds is a dead zone: neither
// direction moves.
func TestControllerHoldsWithinLossDeadBand(t *testing.T) {
	sel := makeLadderSelector(t)
	require.NoError(t, sel.Select(1))
	c := newABRController(sel)

	for range abrDowngradeConfirmations + abrUpgradeConfirmations + 3 {
		_, ok := c.evaluate(3, 30) // 1 < 3 < 5
		require.False(t, ok, "switched while inside the loss dead band")
	}
}

// A climbing RTT is congestion even with zero loss (throttling queues packets
// instead of dropping them), so it downgrades on its own.
func TestControllerDowngradesWhenRTTClimbs(t *testing.T) {
	sel := makeLadderSelector(t)
	require.NoError(t, sel.Select(0)) // top
	c := newABRController(sel)

	// Baseline of 30ms (stable limit 39ms, congested limit 60ms).
	_, ok := c.evaluate(0, 30)
	require.False(t, ok)

	// RTT far above the baseline: first congested sample, no switch yet.
	_, ok = c.evaluate(0, 200)
	require.False(t, ok)

	// Second congested sample: downgrade one step, with zero packet loss.
	target, ok := c.evaluate(0, 200)
	require.True(t, ok)
	require.Equal(t, 1, target)
}

// An RTT between the upgrade-stable and congestion thresholds is a dead zone:
// neither direction moves.
func TestControllerHoldsWhenRTTBetweenStableAndCongested(t *testing.T) {
	sel := makeLadderSelector(t)
	require.NoError(t, sel.Select(1))
	c := newABRController(sel)

	_, _ = c.evaluate(0, 30) // baseline 30ms

	// 50ms is past the stable limit (39ms) but under the congested limit (60ms).
	for range abrDowngradeConfirmations + abrUpgradeConfirmations + 3 {
		_, ok := c.evaluate(0, 50)
		require.False(t, ok, "switched while the RTT was between stable and congested")
	}
}

func TestControllerNoSwitchAtLadderEnds(t *testing.T) {
	top := makeLadderSelector(t)
	require.NoError(t, top.Select(0))
	cTop := newABRController(top)
	for range abrUpgradeConfirmations + 2 {
		_, ok := cTop.evaluate(0, 30)
		require.False(t, ok, "cannot upgrade past the top layer")
	}

	bottom := makeLadderSelector(t)
	require.NoError(t, bottom.Select(2))
	cBottom := newABRController(bottom)
	for range abrDowngradeConfirmations + 2 {
		_, ok := cBottom.evaluate(10, 30)
		require.False(t, ok, "cannot downgrade past the bottom layer")
	}
}

// A decision in one direction must not inherit the other direction's
// progress.
func TestControllerCountersResetOnDirectionChange(t *testing.T) {
	sel := makeLadderSelector(t)
	require.NoError(t, sel.Select(1))
	c := newABRController(sel)

	for range abrUpgradeConfirmations - 1 {
		_, ok := c.evaluate(0, 30)
		require.False(t, ok)
	}

	// A lossy sample flips direction and discards the upgrade streak.
	_, ok := c.evaluate(10, 30)
	require.False(t, ok)

	_, ok = c.evaluate(0, 30)
	require.False(t, ok, "upgrade fired on the first sample after a direction change")
}

func TestControllerResetClearsCounters(t *testing.T) {
	sel := makeLadderSelector(t)
	require.NoError(t, sel.Select(2))
	c := newABRController(sel)

	for range abrUpgradeConfirmations - 1 {
		_, ok := c.evaluate(0, 30)
		require.False(t, ok)
	}

	c.reset()

	_, ok := c.evaluate(0, 30)
	require.False(t, ok, "upgrade fired on the first sample after reset()")
}

func TestVideoLadderSortsLowToHigh(t *testing.T) {
	sel := makeLadderSelector(t)
	ladder := videoLadder(sel.GetTracks())
	require.Len(t, ladder, 3)
	require.Equal(t, []int{2, 1, 0}, []int{ladder[0].ID, ladder[1].ID, ladder[2].ID})

	// step moves exactly one rung and refuses to fall off either end.
	id, ok := step(ladder, 2, +1)
	require.True(t, ok)
	require.Equal(t, 1, id)

	id, ok = step(ladder, 0, -1)
	require.True(t, ok)
	require.Equal(t, 1, id)

	_, ok = step(ladder, 0, +1)
	require.False(t, ok)
	_, ok = step(ladder, 2, -1)
	require.False(t, ok)
	_, ok = step(ladder, 99, -1)
	require.False(t, ok, "an unknown active track has no ladder position")
}

func TestControllerNoSwitchWithoutVideoLadder(t *testing.T) {
	desc := &description.Session{
		Medias: []*description.Media{
			{Type: description.MediaTypeAudio, Formats: []format.Format{&format.Opus{PayloadTyp: 111}}},
		},
	}
	sel := webrtcproto.NewTrackSelector(nil, func(_, _ int) {})
	require.NoError(t, sel.LoadFromDescription(desc))
	c := newABRController(sel)

	_, ok := c.evaluate(10, 30)
	require.False(t, ok)
}
