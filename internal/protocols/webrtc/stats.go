package webrtc

// Stats are WebRTC statistics.
type Stats struct {
	BytesReceived       uint64
	BytesSent           uint64
	RTPPacketsReceived  uint64
	RTPPacketsSent      uint64
	RTPPacketsLost      uint64
	RTPPacketsJitter    float64
	RTCPPacketsReceived uint64
	RTCPPacketsSent     uint64
	// NACKPacketsRequested/NACKPacketsReceived count individual RTP
	// sequence numbers asked for retransmission by this peer / by the
	// remote peer - see statsInterceptor's fields.
	NACKPacketsRequested uint64
	NACKPacketsReceived  uint64
	// RTTMilliseconds is the selected ICE candidate pair's latest
	// round-trip time, the counterpart of SRT's MsRTT.
	RTTMilliseconds float64
}
