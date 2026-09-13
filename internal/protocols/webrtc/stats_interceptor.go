package webrtc

import (
	"sync/atomic"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
)

type statsInterceptor struct {
	rtcpPacketsSent     atomic.Uint64
	rtcpPacketsReceived atomic.Uint64
	// nackPacketsRequested counts individual RTP sequence numbers this
	// peer asked the remote to retransmit (NACK), not NACK RTCP packets:
	// one TransportLayerNack can name many packets via its bitmask. This
	// is the direct measure of "how much did this hop actually lose and
	// try to recover", which raw RTPPacketsLost can't distinguish from
	// loss that NACK repaired in time.
	nackPacketsRequested atomic.Uint64
	// nackPacketsReceived counts the same, but for NACKs arriving from
	// the remote peer - i.e. loss on our *outbound* direction.
	nackPacketsReceived atomic.Uint64
}

func (*statsInterceptor) Close() error {
	return nil
}

func (s *statsInterceptor) BindRTCPReader(reader interceptor.RTCPReader) interceptor.RTCPReader {
	return interceptor.RTCPReaderFunc(func(bytes []byte,
		attributes interceptor.Attributes,
	) (int, interceptor.Attributes, error) {
		n, attrs, err := reader.Read(bytes, attributes)

		pkts, err2 := attrs.GetRTCPPackets(bytes)
		if err2 == nil {
			s.rtcpPacketsReceived.Add(uint64(len(pkts)))
			s.nackPacketsReceived.Add(countNackedPackets(pkts))
		}

		return n, attrs, err
	})
}

// countNackedPackets sums the number of distinct RTP sequence numbers named
// across every TransportLayerNack in pkts (each NackPair covers a base
// sequence number plus a 16-bit bitmask of following ones).
func countNackedPackets(pkts []rtcp.Packet) uint64 {
	n := uint64(0)
	for _, pkt := range pkts {
		if nack, ok := pkt.(*rtcp.TransportLayerNack); ok {
			for i := range nack.Nacks {
				n += uint64(len(nack.Nacks[i].PacketList()))
			}
		}
	}
	return n
}

func (s *statsInterceptor) BindRTCPWriter(writer interceptor.RTCPWriter) interceptor.RTCPWriter {
	return interceptor.RTCPWriterFunc(func(pkts []rtcp.Packet, attributes interceptor.Attributes) (int, error) {
		s.rtcpPacketsSent.Add(uint64(len(pkts)))
		s.nackPacketsRequested.Add(countNackedPackets(pkts))
		return writer.Write(pkts, attributes)
	})
}

func (s *statsInterceptor) BindLocalStream(_ *interceptor.StreamInfo,
	writer interceptor.RTPWriter,
) interceptor.RTPWriter {
	return writer
}

func (*statsInterceptor) UnbindLocalStream(_ *interceptor.StreamInfo) {}

func (s *statsInterceptor) BindRemoteStream(_ *interceptor.StreamInfo,
	reader interceptor.RTPReader,
) interceptor.RTPReader {
	return reader
}

func (*statsInterceptor) UnbindRemoteStream(_ *interceptor.StreamInfo) {}

type statsInterceptorFactory struct {
	onCreate func(s *statsInterceptor)
}

func (f *statsInterceptorFactory) NewInterceptor(_ string) (interceptor.Interceptor, error) {
	s := &statsInterceptor{}

	f.onCreate(s)

	return s, nil
}
