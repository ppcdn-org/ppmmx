package webrtc

import (
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/stretchr/testify/require"
)

func makeSimulcastDesc() *description.Session {
	return &description.Session{
		Medias: []*description.Media{
			{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PayloadTyp: 96, PacketizationMode: 1}}},
			{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PayloadTyp: 96, PacketizationMode: 1}}},
			{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PayloadTyp: 96, PacketizationMode: 1}}},
			{Type: description.MediaTypeAudio, Formats: []format.Format{&format.Opus{PayloadTyp: 111}}},
		},
	}
}

func TestTrackSelectorLoadFromDescription(t *testing.T) {
	desc := makeSimulcastDesc()
	sel := NewTrackSelector(nil, func(from, to int) {})
	err := sel.LoadFromDescription(desc)
	require.NoError(t, err)

	tracks := sel.GetTracks()
	require.Len(t, tracks, 4)

	require.Equal(t, "0", tracks[0].RID)
	require.Equal(t, "1", tracks[1].RID)
	require.Equal(t, "2", tracks[2].RID)
	require.Equal(t, "video", tracks[0].Type)
	require.Equal(t, "h264", tracks[0].Codec)
	require.Equal(t, 1920, tracks[0].Width)
	require.Equal(t, 1080, tracks[0].Height)

	require.Equal(t, 999, tracks[3].ID)
	require.Equal(t, "audio", tracks[3].Type)
	require.Equal(t, "opus", tracks[3].Codec)
	require.Equal(t, 0, sel.ActiveTrackID())
	require.Equal(t, 3, sel.VideoTrackCount())
}

func TestTrackSelectorSelectSameActiveIsIdempotent(t *testing.T) {
	desc := makeSimulcastDesc()
	switchCount := 0
	sel := NewTrackSelector(nil, func(from, to int) { switchCount++ })
	err := sel.LoadFromDescription(desc)
	require.NoError(t, err)

	err = sel.Select(0)
	require.NoError(t, err)
	require.Equal(t, 1, switchCount)
	require.Equal(t, 0, sel.ActiveTrackID())
	require.Equal(t, -1, sel.PendingTrackID())
}

func TestTrackSelectorSwitchOnKeyframe(t *testing.T) {
	desc := makeSimulcastDesc()
	var switchedFrom, switchedTo int
	sel := NewTrackSelector(nil, func(from, to int) {
		switchedFrom = from
		switchedTo = to
	})
	err := sel.LoadFromDescription(desc)
	require.NoError(t, err)

	err = sel.Select(1)
	require.NoError(t, err)
	require.Equal(t, 1, sel.PendingTrackID())
	require.Equal(t, 0, sel.ActiveTrackID())

	// non-keyframe AU of the pending track: no switch, not forwarded
	shouldForward, switched := sel.OnAccessUnit(1, false)
	require.False(t, shouldForward)
	require.False(t, switched)
	require.Equal(t, 0, sel.ActiveTrackID())

	// active track keeps forwarding while the switch is pending
	shouldForward, switched = sel.OnAccessUnit(0, false)
	require.True(t, shouldForward)
	require.False(t, switched)

	// keyframe AU of the pending track completes the switch
	shouldForward, switched = sel.OnAccessUnit(1, true)
	require.True(t, shouldForward)
	require.True(t, switched)
	require.Equal(t, 1, sel.ActiveTrackID())
	require.Equal(t, -1, sel.PendingTrackID())
	require.Equal(t, 0, switchedFrom)
	require.Equal(t, 1, switchedTo)

	// former active track is no longer forwarded
	shouldForward, _ = sel.OnAccessUnit(0, false)
	require.False(t, shouldForward)
}

func TestTrackSelectorActiveTrackForwardsDirectly(t *testing.T) {
	desc := makeSimulcastDesc()
	sel := NewTrackSelector(nil, nil)
	err := sel.LoadFromDescription(desc)
	require.NoError(t, err)

	shouldForward, switched := sel.OnAccessUnit(0, false)
	require.True(t, shouldForward)
	require.False(t, switched)
}

func TestTrackSelectorShouldQueue(t *testing.T) {
	sel := NewTrackSelector(nil, nil)
	require.NoError(t, sel.LoadFromDescription(makeSimulcastDesc()))

	require.True(t, sel.ShouldQueue(0))
	require.False(t, sel.ShouldQueue(1))
	require.False(t, sel.ShouldQueue(2))

	require.NoError(t, sel.Select(2))
	require.True(t, sel.ShouldQueue(0))
	require.False(t, sel.ShouldQueue(1))
	require.True(t, sel.ShouldQueue(2))

	shouldForward, switched := sel.OnAccessUnit(2, true)
	require.True(t, shouldForward)
	require.True(t, switched)
	require.False(t, sel.ShouldQueue(0))
	require.True(t, sel.ShouldQueue(2))
}

func TestTrackSelectorRejectInvalidTrack(t *testing.T) {
	desc := makeSimulcastDesc()
	sel := NewTrackSelector(nil, nil)
	err := sel.LoadFromDescription(desc)
	require.NoError(t, err)

	err = sel.Select(999)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a video track")

	err = sel.Select(99)
	require.Error(t, err)
}

func TestTrackSelectorSelectOverridesPending(t *testing.T) {
	desc := makeSimulcastDesc()
	sel := NewTrackSelector(nil, nil)
	err := sel.LoadFromDescription(desc)
	require.NoError(t, err)

	err = sel.Select(1)
	require.NoError(t, err)
	require.Equal(t, 1, sel.PendingTrackID())

	// a new request replaces the pending target instead of failing
	err = sel.Select(2)
	require.NoError(t, err)
	require.Equal(t, 2, sel.PendingTrackID())

	// keyframe of the superseded target doesn't switch
	shouldForward, switched := sel.OnAccessUnit(1, true)
	require.False(t, shouldForward)
	require.False(t, switched)
	require.Equal(t, 0, sel.ActiveTrackID())

	shouldForward, switched = sel.OnAccessUnit(2, true)
	require.True(t, shouldForward)
	require.True(t, switched)
	require.Equal(t, 2, sel.ActiveTrackID())
}

func TestTrackSelectorPendingTimeout(t *testing.T) {
	desc := makeSimulcastDesc()
	var switchedFrom, switchedTo int
	callbacks := 0
	sel := NewTrackSelector(nil, func(from, to int) {
		switchedFrom = from
		switchedTo = to
		callbacks++
	})
	err := sel.LoadFromDescription(desc)
	require.NoError(t, err)

	err = sel.Select(1)
	require.NoError(t, err)
	require.Equal(t, 1, sel.PendingTrackID())

	// simulate an expired pending switch
	sel.mu.Lock()
	sel.pendingAt = time.Now().Add(-pendingTimeout - time.Second)
	sel.mu.Unlock()

	// the timeout is evaluated on ANY track's AU, including the active one,
	// so a stalled target track cannot wedge the selector
	shouldForward, switched := sel.OnAccessUnit(0, false)
	require.True(t, shouldForward)
	require.False(t, switched)
	require.Equal(t, -1, sel.PendingTrackID())
	require.Equal(t, 0, sel.ActiveTrackID())

	// the client is resynced onto the current track
	require.Equal(t, 1, callbacks)
	require.Equal(t, 0, switchedFrom)
	require.Equal(t, 0, switchedTo)
}

func TestPipelineDelay(t *testing.T) {
	sel := NewTrackSelector(nil, nil)
	require.Equal(t, 10*time.Millisecond, sel.PipelineDelay())
}
