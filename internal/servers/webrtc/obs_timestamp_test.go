package webrtc

import (
	"strconv"
	"testing"
	"time"

	pwebrtc "github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"

	webrtcproto "github.com/bluenviron/mediamtx/internal/protocols/webrtc"
	"github.com/bluenviron/mediamtx/internal/test"
)

func testSession() *session {
	return &session{parent: &Server{Parent: test.NilLogger}}
}

func TestOnInboundDataChannelIgnoresOtherLabels(t *testing.T) {
	sx := testSession()

	// Simulate registration for a channel with a different label: the
	// handler should return before calling dc.OnMessage at all, so there's
	// nothing to invoke and obsTimestampLatencyMs must stay nil.
	dc, err := pwebrtc.NewPeerConnection(pwebrtc.Configuration{})
	require.NoError(t, err)
	defer dc.Close() //nolint:errcheck

	ch, err := dc.CreateDataChannel("some-other-channel", nil)
	require.NoError(t, err)

	sx.onInboundDataChannel(ch)

	sx.mutex.RLock()
	defer sx.mutex.RUnlock()
	require.Nil(t, sx.obsTimestampLatencyMs)
}

func TestOnInboundDataChannelParsesTimestampMessage(t *testing.T) {
	sx := testSession()

	pub, err := pwebrtc.NewPeerConnection(pwebrtc.Configuration{})
	require.NoError(t, err)
	defer pub.Close() //nolint:errcheck

	pubDC, err := pub.CreateDataChannel("obs-timestamp", nil)
	require.NoError(t, err)

	dataChanReady := make(chan *pwebrtc.DataChannel, 1)

	reader := &webrtcproto.PeerConnection{
		LocalRandomUDP:    true,
		IPsFromInterfaces: true,
		Publish:           false,
		OnInboundDataChannel: func(d *pwebrtc.DataChannel) {
			dataChanReady <- d
		},
		Log: test.NilLogger,
	}
	require.NoError(t, reader.Start())
	defer reader.Close()

	offer, err := pub.CreateOffer(nil)
	require.NoError(t, err)
	require.NoError(t, pub.SetLocalDescription(offer))

	answer, err := reader.CreateFullAnswer(&offer, false)
	require.NoError(t, err)
	require.NoError(t, pub.SetRemoteDescription(*answer))

	require.NoError(t, reader.WaitUntilConnected(10*time.Second))

	dc := <-dataChanReady
	sx.onInboundDataChannel(dc)

	require.Eventually(t, func() bool {
		return pubDC.ReadyState() == pwebrtc.DataChannelStateOpen
	}, 10*time.Second, 10*time.Millisecond)

	sentTS := uint64(time.Now().Add(-42 * time.Millisecond).UnixMilli())
	require.NoError(t, pubDC.SendText(`{"frame_no":7,"timestamp":`+
		strconv.FormatUint(sentTS, 10)+`,"rid":"0"}`))

	require.Eventually(t, func() bool {
		sx.mutex.RLock()
		defer sx.mutex.RUnlock()
		return sx.obsTimestampLatencyMs != nil
	}, 5*time.Second, 10*time.Millisecond)

	sx.mutex.RLock()
	latency := *sx.obsTimestampLatencyMs
	sx.mutex.RUnlock()
	require.InDelta(t, 42, latency, 30) // generous tolerance for CI jitter
}
