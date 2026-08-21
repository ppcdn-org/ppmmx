package webrtc

import (
	"testing"

	"github.com/pion/sdp/v3"
	"github.com/stretchr/testify/require"
)

func videoMediaWithRIDs(rids ...string) *sdp.MediaDescription {
	m := &sdp.MediaDescription{MediaName: sdp.MediaName{Media: "video"}}
	for _, rid := range rids {
		m.Attributes = append(m.Attributes, sdp.Attribute{Key: "rid", Value: rid + " send"})
	}
	return m
}

func TestOfferVideoSimulcastLayerCount(t *testing.T) {
	// no rid attributes at all: a single (non-Simulcast) video layer.
	require.Equal(t, 1, offerVideoSimulcastLayerCount([]*sdp.MediaDescription{videoMediaWithRIDs()}))

	// OBS puts every layer's rid on the same (single) video m-line.
	require.Equal(t, 3, offerVideoSimulcastLayerCount([]*sdp.MediaDescription{videoMediaWithRIDs("0", "1", "2")}))
	require.Equal(t, 5, offerVideoSimulcastLayerCount([]*sdp.MediaDescription{
		videoMediaWithRIDs("0", "1", "2", "3", "4"),
	}))

	// audio/application media never counts.
	require.Equal(t, 2, offerVideoSimulcastLayerCount([]*sdp.MediaDescription{
		videoMediaWithRIDs("0", "1"),
		{MediaName: sdp.MediaName{Media: "audio"}},
		{MediaName: sdp.MediaName{Media: "application"}},
	}))
}

func TestMaxWHIPSimulcastLayersRejectsOverLimit(t *testing.T) {
	medias := []*sdp.MediaDescription{videoMediaWithRIDs("0", "1", "2", "3", "4")}
	require.Equal(t, maxWHIPSimulcastLayers, offerVideoSimulcastLayerCount(medias))

	tooMany := []*sdp.MediaDescription{videoMediaWithRIDs("0", "1", "2", "3", "4", "5")}
	require.Greater(t, offerVideoSimulcastLayerCount(tooMany), maxWHIPSimulcastLayers)
}
