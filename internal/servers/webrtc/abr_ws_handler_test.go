package webrtc

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// A nil selector means a multi-track/simulcast WHEP session (see session.go)
// rather than an error, so the message must still be well-formed - a
// zero-value abrMessage (Type=="") leaves the client's Quality dropdown
// stuck on "Loading..." forever, since it never matches a known message type.
func TestTracksInfoMessageWithNilSelectorIsWellFormed(t *testing.T) {
	msg := tracksInfoMessage(nil)
	require.Equal(t, "TRACKS_INFO", msg.Type)

	var data struct {
		ActiveTrackID int           `json:"active_track_id"`
		Tracks        []interface{} `json:"tracks"`
	}
	require.NoError(t, json.Unmarshal(msg.Data, &data))
	require.Equal(t, -1, data.ActiveTrackID)
	require.NotNil(t, data.Tracks)
	require.Empty(t, data.Tracks)
}

// ABR_RECOMMEND must carry the reserved reason the client is expected to
// echo back on its resulting SELECT_LAYER (see selectLayerKeepsAutoMode) -
// a typo or drift here would silently break the "stay in auto mode" contract
// between server and client despite both sides individually compiling fine.
func TestAbrRecommendMessageCarriesAutoBandwidthReason(t *testing.T) {
	msg := abrRecommendMessage(2)
	require.Equal(t, "ABR_RECOMMEND", msg.Type)

	var data abrRecommendData
	require.NoError(t, json.Unmarshal(msg.Data, &data))
	require.Equal(t, 2, data.TargetTrackID)
	require.Equal(t, abrReasonAutoBandwidth, data.Reason)
}

// The one and only reason that must survive as automatic mode is the
// client executing the server's own recommendation. Every other value -
// including the empty string an old/buggy client might send - is a
// genuine manual pick and must exit auto mode, matching the pre-existing
// "any explicit SELECT_LAYER means the viewer took over" behavior.
func TestSelectLayerKeepsAutoModeOnlyForAutoBandwidthReason(t *testing.T) {
	require.True(t, selectLayerKeepsAutoMode(abrReasonAutoBandwidth))
	require.False(t, selectLayerKeepsAutoMode("manual_user"))
	require.False(t, selectLayerKeepsAutoMode("user_resume"))
	require.False(t, selectLayerKeepsAutoMode(""))
}
