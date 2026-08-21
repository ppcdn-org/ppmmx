package webrtc

import (
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/rtpreceiver"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/bluenviron/mediamtx/internal/counterdumper"
	"github.com/bluenviron/mediamtx/internal/logger"
)

const (
	keyFrameInterval      = 2 * time.Second
	inboundVideoQueueSize = 512
	mimeTypeMultiopus     = "audio/multiopus"
	mimeTypeL16           = "audio/L16"
)

func inboundTrackTWCCExtensionID(params webrtc.RTPParameters) uint8 {
	for _, ext := range params.HeaderExtensions {
		if ext.URI == twccExtensionURI {
			return uint8(ext.ID)
		}
	}
	return 0
}

var incomingVideoCodecs = []webrtc.RTPCodecParameters{
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeAV1,
			ClockRate:   90000,
			SDPFmtpLine: "profile=1",
		},
		PayloadType: 96,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeAV1,
			ClockRate: 90000,
		},
		PayloadType: 97,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeVP9,
			ClockRate:   90000,
			SDPFmtpLine: "profile-id=3",
		},
		PayloadType: 98,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeVP9,
			ClockRate:   90000,
			SDPFmtpLine: "profile-id=2",
		},
		PayloadType: 99,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeVP9,
			ClockRate:   90000,
			SDPFmtpLine: "profile-id=1",
		},
		PayloadType: 100,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeVP9,
			ClockRate:   90000,
			SDPFmtpLine: "profile-id=0",
		},
		PayloadType: 101,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeVP8,
			ClockRate: 90000,
		},
		PayloadType: 102,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH265,
			ClockRate:   90000,
			SDPFmtpLine: "level-id=93;profile-id=2;tier-flag=0;tx-mode=SRST",
		},
		PayloadType: 103,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH265,
			ClockRate:   90000,
			SDPFmtpLine: "level-id=93;profile-id=1;tier-flag=0;tx-mode=SRST",
		},
		PayloadType: 104,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f",
		},
		PayloadType: 105,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
		},
		PayloadType: 106,
	},
}

var incomingAudioCodecs = []webrtc.RTPCodecParameters{
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    mimeTypeMultiopus,
			ClockRate:   48000,
			Channels:    3,
			SDPFmtpLine: "channel_mapping=0,2,1;num_streams=2;coupled_streams=1",
		},
		PayloadType: 112,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    mimeTypeMultiopus,
			ClockRate:   48000,
			Channels:    4,
			SDPFmtpLine: "channel_mapping=0,1,2,3;num_streams=2;coupled_streams=2",
		},
		PayloadType: 113,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    mimeTypeMultiopus,
			ClockRate:   48000,
			Channels:    5,
			SDPFmtpLine: "channel_mapping=0,4,1,2,3;num_streams=3;coupled_streams=2",
		},
		PayloadType: 114,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    mimeTypeMultiopus,
			ClockRate:   48000,
			Channels:    6,
			SDPFmtpLine: "channel_mapping=0,4,1,2,3,5;num_streams=4;coupled_streams=2",
		},
		PayloadType: 115,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    mimeTypeMultiopus,
			ClockRate:   48000,
			Channels:    7,
			SDPFmtpLine: "channel_mapping=0,4,1,2,3,5,6;num_streams=4;coupled_streams=4",
		},
		PayloadType: 116,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    mimeTypeMultiopus,
			ClockRate:   48000,
			Channels:    8,
			SDPFmtpLine: "channel_mapping=0,6,1,4,5,2,3,7;num_streams=5;coupled_streams=4",
		},
		PayloadType: 117,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeOpus,
			ClockRate:   48000,
			Channels:    2,
			SDPFmtpLine: "minptime=10;useinbandfec=1;stereo=1;sprop-stereo=1",
		},
		PayloadType: 111,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeG722,
			ClockRate: 8000,
		},
		PayloadType: 9,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypePCMU,
			ClockRate: 8000,
			Channels:  2,
		},
		PayloadType: 118,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypePCMA,
			ClockRate: 8000,
			Channels:  2,
		},
		PayloadType: 119,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypePCMU,
			ClockRate: 8000,
		},
		PayloadType: 0,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypePCMA,
			ClockRate: 8000,
		},
		PayloadType: 8,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  mimeTypeL16,
			ClockRate: 8000,
			Channels:  2,
		},
		PayloadType: 120,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  mimeTypeL16,
			ClockRate: 16000,
			Channels:  2,
		},
		PayloadType: 121,
	},
	{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  mimeTypeL16,
			ClockRate: 48000,
			Channels:  2,
		},
		PayloadType: 122,
	},
}

// InboundTrack is an incoming track.
type InboundTrack struct {
	OnPacketRTP func(*rtp.Packet)

	track     *webrtc.TrackRemote
	receiver  *webrtc.RTPReceiver
	id        string
	rid       string
	writeRTCP func([]rtcp.Packet) error
	log       logger.Writer

	twccExtID                uint8
	inboundRTPPacketsLost    *counterdumper.Dumper
	inboundRTPPacketsDropped *counterdumper.Dumper
	rtpReceiver              *rtpreceiver.Receiver
	done                     chan struct{}
	lastInputSequence        uint16
	hasLastInputSequence     bool
}

func (t *InboundTrack) initialize() {
	t.OnPacketRTP = func(*rtp.Packet) {}
	t.twccExtID = inboundTrackTWCCExtensionID(t.receiver.GetParameters())
}

func (t *InboundTrack) stripTWCCExtension(pkt *rtp.Packet) {
	if t.twccExtID == 0 || pkt.GetExtension(t.twccExtID) == nil {
		return
	}

	err := pkt.DelExtension(t.twccExtID)
	if err != nil {
		panic(err)
	}

	if len(pkt.GetExtensionIDs()) == 0 {
		pkt.Extension = false
		pkt.ExtensionProfile = 0
	}
}

// Codec returns the track codec.
func (t *InboundTrack) Codec() webrtc.RTPCodecParameters {
	return t.track.Codec()
}

// Kind returns whether this is an audio or video track.
func (t *InboundTrack) Kind() webrtc.RTPCodecType {
	return t.track.Kind()
}

// Stats returns this track's RTP receive statistics.
func (t *InboundTrack) Stats() *rtpreceiver.Stats {
	return t.rtpReceiver.Stats()
}

// ClockRate returns the clock rate. Needed by rtptime.GlobalDecoder
func (t *InboundTrack) ClockRate() int {
	return int(t.track.Codec().ClockRate)
}

// PTSEqualsDTS returns whether PTS equals DTS. Needed by rtptime.GlobalDecoder
func (*InboundTrack) PTSEqualsDTS(*rtp.Packet) bool {
	return true
}

func (t *InboundTrack) start() {
	t.done = make(chan struct{})
	t.inboundRTPPacketsLost = &counterdumper.Dumper{
		OnReport: func(val uint64) {
			t.log.Log(logger.Warn, "%d RTP %s lost",
				val,
				func() string {
					if val == 1 {
						return "packet"
					}
					return "packets"
				}())
		},
	}
	t.inboundRTPPacketsLost.Start()

	t.inboundRTPPacketsDropped = &counterdumper.Dumper{
		OnReport: func(val uint64) {
			t.log.Log(logger.Warn, "discarded %d queued RTP packets to keep the WHIP input live", val)
		},
	}
	t.inboundRTPPacketsDropped.Start()

	t.rtpReceiver = &rtpreceiver.Receiver{
		ClockRate:            int(t.track.Codec().ClockRate),
		UnrealiableTransport: true,
		Period:               1 * time.Second,
		WritePacketRTCP: func(p rtcp.Packet) {
			t.writeRTCP([]rtcp.Packet{p}) //nolint:errcheck
		},
	}
	err := t.rtpReceiver.Initialize()
	if err != nil {
		panic(err)
	}

	// read incoming RTCP packets.
	// incoming RTCP packets must always be read to make interceptors work.
	go func() {
		buf := make([]byte, 1500)
		for {
			n, _, err2 := t.receiver.ReadSimulcast(buf, t.rid)
			if err2 != nil {
				return
			}

			pkts, err2 := rtcp.Unmarshal(buf[:n])
			if err2 != nil {
				panic(err2)
			}

			for _, pkt := range pkts {
				if sr, ok := pkt.(*rtcp.SenderReport); ok {
					t.rtpReceiver.ProcessSenderReport(sr, time.Now())
				}
			}
		}
	}()

	// send period key frame requests
	if t.track.Kind() == webrtc.RTPCodecTypeVideo {
		go func() {
			keyframeTicker := time.NewTicker(keyFrameInterval)
			defer keyframeTicker.Stop()

			for range keyframeTicker.C {
				err2 := t.writeRTCP([]rtcp.Packet{
					&rtcp.PictureLossIndication{
						MediaSSRC: uint32(t.track.SSRC()),
					},
				})
				if err2 != nil {
					return
				}
			}
		}()
	}

	processPacket := func(pkt *rtp.Packet) {
		previousSequence := t.lastInputSequence
		hadPreviousSequence := t.hasLastInputSequence
		t.lastInputSequence = pkt.SequenceNumber
		t.hasLastInputSequence = true
		packets, lost := t.rtpReceiver.ProcessPacket2(pkt, time.Now(), true)

		if lost != 0 {
			t.log.Log(logger.Warn,
				"RTP loss diagnostic: lost=%d ssrc=%d rid=%q pt=%d previousInputSeq=%d currentSeq=%d timestamp=%d marker=%t hadPrevious=%t",
				lost, pkt.SSRC, t.rid, pkt.PayloadType, previousSequence, pkt.SequenceNumber,
				pkt.Timestamp, pkt.Marker, hadPreviousSequence)
			t.inboundRTPPacketsLost.Add(lost)
			// do not return
		}

		for _, pkt := range packets {
			// sometimes Chrome sends empty RTP packets. ignore them.
			if len(pkt.Payload) == 0 {
				continue
			}

			t.stripTWCCExtension(pkt)
			t.OnPacketRTP(pkt)
		}
	}

	// Video processing can be temporarily slower than Pion's socket reader
	// during multi-layer IDR bursts. Drain Pion continuously into a bounded
	// per-layer queue; if it fills, discard stale packets and return to the
	// live edge instead of accumulating minutes of latency.
	if t.track.Kind() == webrtc.RTPCodecTypeVideo {
		queue := make(chan *rtp.Packet, inboundVideoQueueSize)

		go func() {
			for {
				select {
				case pkt := <-queue:
					processPacket(pkt)
				case <-t.done:
					return
				}
			}
		}()

		go func() {
			for {
				pkt, _, err2 := t.track.ReadRTP()
				if err2 != nil {
					return
				}

				select {
				case queue <- pkt:
				default:
					dropped := uint64(0)
					draining := true
					for draining {
						select {
						case <-queue:
							dropped++
						default:
							draining = false
						}
					}
					t.inboundRTPPacketsDropped.Add(dropped)
					queue <- pkt
				}
			}
		}()
		return
	}

	// Audio has a much lower packet rate and is kept on the direct path.
	go func() {
		for {
			pkt, _, err2 := t.track.ReadRTP()
			if err2 != nil {
				return
			}
			processPacket(pkt)
		}
	}()
}

// PacketNTP returns the packet NTP.
func (t *InboundTrack) PacketNTP(pkt *rtp.Packet) (time.Time, bool) {
	return t.rtpReceiver.PacketNTP(pkt.Timestamp)
}

func (t *InboundTrack) close() {
	if t.done != nil {
		close(t.done)
		t.done = nil
	}
	if t.inboundRTPPacketsLost != nil {
		t.inboundRTPPacketsLost.Stop()
	}
	if t.inboundRTPPacketsDropped != nil {
		t.inboundRTPPacketsDropped.Stop()
	}
	if t.rtpReceiver != nil {
		t.rtpReceiver.Close()
	}
}
