package webrtc

import "encoding/json"

// OBSTimestampDataChannelLabel is the WebRTC DataChannel label OBS's WHIP
// output creates to broadcast a per-frame timestamp alongside the stream,
// as described in docs/obs-abs-timestamp-protocol.md (OBS repo, section
// 3). The channel is created by OBS (the publisher); mmx only has to
// listen for it via PeerConnection.OnInboundDataChannel, not create it.
const OBSTimestampDataChannelLabel = "obs-timestamp"

// OBSTimestampMessage is a single message sent over the "obs-timestamp"
// DataChannel: one per video frame per simulcast layer.
type OBSTimestampMessage struct {
	// FrameNo is the RTP sequence number of the first packet of this
	// frame, within this layer's own sequence number space (not a global
	// frame counter - see the protocol doc). Wraps at 65536.
	FrameNo uint16 `json:"frame_no"`

	// TimestampMS is the sender's NTP-synchronized wall clock time, in
	// milliseconds since the Unix epoch, at the moment this frame was
	// sent.
	TimestampMS uint64 `json:"timestamp"`

	// RID identifies which simulcast layer this message belongs to
	// ("0", "1", "2", ...), matching the RTP "rid" extension value used
	// by that layer. Required to disambiguate FrameNo, which is only
	// unique within a single layer.
	RID string `json:"rid"`
}

// ParseOBSTimestampMessage decodes a single "obs-timestamp" DataChannel
// message (UTF-8 JSON text, see docs/obs-abs-timestamp-protocol.md
// section 3).
func ParseOBSTimestampMessage(data []byte) (*OBSTimestampMessage, error) {
	var msg OBSTimestampMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}
