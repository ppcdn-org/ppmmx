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

func TestMaxHEVCSimulcastLayersIsStricterThanTheH264Ceiling(t *testing.T) {
	atLimit := []*sdp.MediaDescription{videoMediaWithRIDs("0", "1", "2")}
	require.Equal(t, maxHEVCSimulcastLayers, offerVideoSimulcastLayerCount(atLimit))
	require.LessOrEqual(t, offerVideoSimulcastLayerCount(atLimit), maxHEVCSimulcastLayers)

	// 4 layers is still within the generic H264 ceiling (5) but must be
	// rejected on a /hevc path - see runPublish's codec-specific check.
	tooManyForHEVC := []*sdp.MediaDescription{videoMediaWithRIDs("0", "1", "2", "3")}
	require.LessOrEqual(t, offerVideoSimulcastLayerCount(tooManyForHEVC), maxWHIPSimulcastLayers)
	require.Greater(t, offerVideoSimulcastLayerCount(tooManyForHEVC), maxHEVCSimulcastLayers)
}

func TestPathCodecFromName(t *testing.T) {
	require.Equal(t, "h264", pathCodecFromName("testapp/teststream/h264"))
	require.Equal(t, "hevc", pathCodecFromName("testapp/teststream/hevc"))
	// Legacy 2-segment path: unconstrained.
	require.Equal(t, "", pathCodecFromName("testapp/teststream"))
	// Anything else unrecognized: also unconstrained, never misread as a codec.
	require.Equal(t, "", pathCodecFromName("testapp/teststream/av1"))
	require.Equal(t, "", pathCodecFromName(""))
}

func TestPathCodecRtpmapName(t *testing.T) {
	require.Equal(t, "H264", pathCodecRtpmapName("h264"))
	require.Equal(t, "H265", pathCodecRtpmapName("hevc"))
	require.Equal(t, "", pathCodecRtpmapName("av1"))
}

func videoMediaWithRtpmap(rtpmapValue string) *sdp.MediaDescription {
	return &sdp.MediaDescription{
		MediaName: sdp.MediaName{Media: "video"},
		Attributes: []sdp.Attribute{
			{Key: "rtpmap", Value: rtpmapValue},
		},
	}
}

func TestOfferHasVideoCodec(t *testing.T) {
	h264Media := videoMediaWithRtpmap("96 H264/90000")
	h265Media := videoMediaWithRtpmap("97 H265/90000")

	require.True(t, offerHasVideoCodec([]*sdp.MediaDescription{h264Media}, "H264"))
	require.False(t, offerHasVideoCodec([]*sdp.MediaDescription{h264Media}, "H265"))
	require.True(t, offerHasVideoCodec([]*sdp.MediaDescription{h265Media}, "H265"))
	require.False(t, offerHasVideoCodec([]*sdp.MediaDescription{h265Media}, "H264"))

	// Case-insensitive, matching offerH264SendTrackCount's own convention.
	require.True(t, offerHasVideoCodec([]*sdp.MediaDescription{videoMediaWithRtpmap("96 h264/90000")}, "H264"))

	// Non-video media never counts, even with a matching rtpmap.
	audioMedia := &sdp.MediaDescription{
		MediaName:  sdp.MediaName{Media: "audio"},
		Attributes: []sdp.Attribute{{Key: "rtpmap", Value: "111 opus/48000/2"}},
	}
	require.False(t, offerHasVideoCodec([]*sdp.MediaDescription{audioMedia}, "opus"))
}
