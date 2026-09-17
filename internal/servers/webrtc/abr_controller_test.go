package webrtc

import (
	"testing"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/stretchr/testify/require"

	webrtcproto "github.com/bluenviron/mediamtx/internal/protocols/webrtc"
)

// Three video layers plus audio, matching layerDefaults in
// track_selector.go: track 0 = 2000kbps, 1 = 1000kbps, 2 = 400kbps.
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

func TestVideoBudgetReservesAudio(t *testing.T) {
	require.Equal(t, 1_000_000-abrAudioReserveBits, videoBudget(1_000_000))
	// Never negative, even when the estimate is below the audio reserve.
	require.Equal(t, 0, videoBudget(50_000))
}

func TestPickHighestFittingLayer(t *testing.T) {
	sel := makeLadderSelector(t)
	tracks := sel.GetTracks()

	// Comfortably above 2000k * 1.2 - top layer.
	id, ok := pick(tracks, 3_000_000)
	require.True(t, ok)
	require.Equal(t, 0, id)

	// Above 1000k * 1.2 but below 2000k * 1.2 - middle layer.
	id, ok = pick(tracks, 1_500_000)
	require.True(t, ok)
	require.Equal(t, 1, id)

	// Above 400k * 1.2 but below 1000k * 1.2 - bottom layer.
	id, ok = pick(tracks, 600_000)
	require.True(t, ok)
	require.Equal(t, 2, id)
}

func TestPickFallsBackToLowestWhenNothingFits(t *testing.T) {
	sel := makeLadderSelector(t)

	// Below even the lowest layer's requirement: something still has to be
	// sent, and the lowest layer is the closest available approximation.
	id, ok := pick(sel.GetTracks(), 10_000)
	require.True(t, ok)
	require.Equal(t, 2, id)
}

func TestPickNoVideoTracks(t *testing.T) {
	desc := &description.Session{
		Medias: []*description.Media{
			{Type: description.MediaTypeAudio, Formats: []format.Format{&format.Opus{PayloadTyp: 111}}},
		},
	}
	sel := webrtcproto.NewTrackSelector(nil, func(_, _ int) {})
	require.NoError(t, sel.LoadFromDescription(desc))

	_, ok := pick(sel.GetTracks(), 5_000_000)
	require.False(t, ok)
}

func TestControllerUpgradeRequiresConfirmations(t *testing.T) {
	sel := makeLadderSelector(t)
	require.NoError(t, sel.Select(2)) // start at the bottom
	c := newABRController(sel)

	// Plenty of bandwidth for the top layer, but a single sample must not
	// move anything.
	for i := range abrUpgradeConfirmations - 1 {
		_, ok := c.evaluate(5_000_000)
		require.Falsef(t, ok, "upgraded after only %d confirmations", i+1)
	}

	target, ok := c.evaluate(5_000_000)
	require.True(t, ok)
	require.Equal(t, 0, target)
}

func TestControllerDowngradeRequiresFewerConfirmations(t *testing.T) {
	sel := makeLadderSelector(t)
	require.NoError(t, sel.Select(0)) // start at the top
	c := newABRController(sel)

	// Collapse to well under the top layer's needs.
	for i := range abrDowngradeConfirmations - 1 {
		_, ok := c.evaluate(500_000)
		require.Falsef(t, ok, "downgraded after only %d confirmations", i+1)
	}

	target, ok := c.evaluate(500_000)
	require.True(t, ok)
	require.Equal(t, 2, target)

	// Downgrades are meant to react faster than upgrades.
	require.Less(t, abrDowngradeConfirmations, abrUpgradeConfirmations)
}

// The band between "not enough headroom to pick this layer" and "genuinely
// short of what this layer needs" is what stops a steady estimate sitting
// just under the upgrade threshold from repeatedly abandoning the layer it
// just settled on.
func TestControllerHoldsLayerWithinHysteresisBand(t *testing.T) {
	sel := makeLadderSelector(t)
	require.NoError(t, sel.Select(1)) // 1000kbps layer
	c := newABRController(sel)

	// Budget covers the layer itself but not the 1.2x headroom pick()
	// would require to choose it afresh.
	estimate := 1_050_000 + abrAudioReserveBits

	for range abrDowngradeConfirmations + 3 {
		_, ok := c.evaluate(estimate)
		require.False(t, ok, "switched while inside the hysteresis band")
	}
}

func TestControllerNoSwitchWhenAlreadyOnTarget(t *testing.T) {
	sel := makeLadderSelector(t)
	require.NoError(t, sel.Select(0))
	c := newABRController(sel)

	for range abrUpgradeConfirmations + 3 {
		_, ok := c.evaluate(5_000_000)
		require.False(t, ok)
	}
}

// A downgrade decision partway through an upgrade streak (and vice versa)
// must not inherit the other direction's progress.
func TestControllerCountersResetOnDirectionChange(t *testing.T) {
	sel := makeLadderSelector(t)
	require.NoError(t, sel.Select(2))
	c := newABRController(sel)

	for range abrUpgradeConfirmations - 1 {
		_, ok := c.evaluate(5_000_000)
		require.False(t, ok)
	}

	// Estimate collapses; already on the lowest layer so there is nothing
	// to drop to, and the pending upgrade progress must be discarded.
	_, ok := c.evaluate(50_000)
	require.False(t, ok)

	_, ok = c.evaluate(5_000_000)
	require.False(t, ok, "upgrade fired on the first sample after a reset")
}

func TestControllerResetClearsCounters(t *testing.T) {
	sel := makeLadderSelector(t)
	require.NoError(t, sel.Select(2))
	c := newABRController(sel)

	for range abrUpgradeConfirmations - 1 {
		_, ok := c.evaluate(5_000_000)
		require.False(t, ok)
	}

	c.reset()

	_, ok := c.evaluate(5_000_000)
	require.False(t, ok, "upgrade fired on the first sample after reset()")
}
