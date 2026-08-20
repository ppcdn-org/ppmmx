package webrtc

import (
	"testing"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/unit"
)

func TestMediaStateIndependentPauseResume(t *testing.T) {
	state := &MediaState{}
	video := &description.Media{Type: description.MediaTypeVideo}
	audio := &description.Media{Type: description.MediaTypeAudio}
	videoUnit := &unit.Unit{Payload: unit.PayloadH264{{0x65}}}
	audioUnit := &unit.Unit{Payload: unit.PayloadOpus{{0x01}}}

	require.True(t, state.Allow(video, videoUnit))
	require.True(t, state.Allow(audio, audioUnit))

	state.SetVideoPaused(true)
	require.False(t, state.Allow(video, videoUnit))
	require.True(t, state.Allow(audio, audioUnit))

	state.SetAudioPaused(true)
	require.False(t, state.Allow(video, videoUnit))
	require.False(t, state.Allow(audio, audioUnit))

	state.SetAudioPaused(false)
	require.False(t, state.Allow(video, videoUnit))
	require.True(t, state.Allow(audio, audioUnit))
}

func TestMediaStateVideoResumeWaitsForIDR(t *testing.T) {
	state := &MediaState{}
	video := &description.Media{Type: description.MediaTypeVideo}
	state.SetVideoPaused(true)
	state.SetVideoPaused(false)

	require.True(t, state.VideoNeedsKeyframe())
	require.False(t, state.Allow(video, &unit.Unit{Payload: unit.PayloadH264{{0x41}}}))
	require.True(t, state.VideoNeedsKeyframe())
	require.True(t, state.Allow(video, &unit.Unit{Payload: unit.PayloadH264{{0x67}, {0x65}}}))
	require.False(t, state.VideoNeedsKeyframe())
	require.True(t, state.Allow(video, &unit.Unit{Payload: unit.PayloadH264{{0x41}}}))
}

func TestMediaStateVideoResumeGatesEachMedia(t *testing.T) {
	state := &MediaState{}
	video1 := &description.Media{Type: description.MediaTypeVideo}
	video2 := &description.Media{Type: description.MediaTypeVideo}
	nonIDR := &unit.Unit{Payload: unit.PayloadH264{{0x41}}}
	idr := &unit.Unit{Payload: unit.PayloadH264{{0x65}}}

	state.SetVideoPaused(true)
	require.False(t, state.Allow(video1, nonIDR))
	require.False(t, state.Allow(video2, nonIDR))
	state.SetVideoPaused(false)

	require.True(t, state.Allow(video1, idr))
	require.False(t, state.Allow(video2, nonIDR))
	require.True(t, state.VideoNeedsKeyframe())
	require.True(t, state.Allow(video2, idr))
	require.False(t, state.VideoNeedsKeyframe())
}
