package webrtc

import (
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/rtpreceiver"
	"github.com/stretchr/testify/require"
)

// TestInboundTrackBufferSizeDefaultAndOverride locks in the gortsplib
// behavior InboundTrack.start relies on for BufferSize (see
// rtpreceiver.Receiver.Initialize): the zero value - what
// PeerConnection.InboundRTPBufferSize/InboundTrack.bufferSize carry when
// webrtcInboundRTPBufferSize is left unset in the YAML - falls back to
// gortsplib's own built-in default of 64 rather than a broken zero-size
// buffer, and an explicit value (128 for origin nodes, see
// bin/conf/origin.local.yml) is honored as-is.
func TestInboundTrackBufferSizeDefaultAndOverride(t *testing.T) {
	for _, ca := range []struct {
		name       string
		bufferSize int
		expected   int
	}{
		{"unset falls back to gortsplib default", 0, 64},
		{"explicit value is honored", 128, 128},
	} {
		t.Run(ca.name, func(t *testing.T) {
			r := &rtpreceiver.Receiver{
				ClockRate:            90000,
				UnrealiableTransport: true,
				BufferSize:           ca.bufferSize,
				Period:               1 * time.Second,
			}
			require.NoError(t, r.Initialize())
			require.Equal(t, ca.expected, r.BufferSize)
		})
	}
}
