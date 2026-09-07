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
