package webrtc

import (
	"fmt"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/stream"
	"github.com/bluenviron/mediamtx/internal/test"
	"github.com/bluenviron/mediamtx/internal/unit"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/stretchr/testify/require"
)

func TestABROutputClock(t *testing.T) {
	start := time.Date(2026, 7, 17, 15, 0, 0, 0, time.UTC)
	clock := abrOutputClock{baseTS: 1234}

	require.Equal(t, uint32(1234), clock.next(start))
	require.Equal(t, uint32(5734), clock.next(start.Add(50*time.Millisecond)))
	require.Equal(t, uint32(10234), clock.next(start.Add(100*time.Millisecond)))
	require.Equal(t, uint32(10235), clock.next(start.Add(100*time.Millisecond)))
	require.Equal(t, uint32(565033939), clock.next(start.Add(15*time.Hour)))
	require.Equal(t, uint32(806066643), clock.next(start.Add(29*time.Hour)))
	require.Equal(t, uint32(806066644), clock.next(start.Add(29*time.Hour)))

	clock = abrOutputClock{baseTS: ^uint32(0) - 1000}
	require.Equal(t, ^uint32(0)-1000, clock.next(start))
	require.Equal(t, uint32(3499), clock.next(start.Add(50*time.Millisecond)))
}

func TestAudioUnitNTP(t *testing.T) {
	start := time.Date(2026, 7, 17, 15, 0, 0, 0, time.UTC)
	sendNow := start.Add(15 * time.Hour)
	sourceNTP := sendNow.Add(-1100 * time.Millisecond)
	calls := 0
	baseNTP := audioUnitNTP(sourceNTP, func() time.Time {
		calls++
		return sendNow
	})

	require.Equal(t, sendNow, baseNTP)
	require.Equal(t, sendNow.Add(20*time.Millisecond), baseNTP.Add(20*time.Millisecond))
	require.Equal(t, 1, calls)
	require.Equal(t, sourceNTP, audioUnitNTP(sourceNTP, nil))
}

func TestFromStreamNoSupportedCodecs(t *testing.T) {
	desc := &description.Session{Medias: []*description.Media{{
		Type:    description.MediaTypeVideo,
		Formats: []format.Format{&format.MJPEG{}},
	}}}

	r := &stream.Reader{
		Parent: test.Logger(func(logger.Level, string, ...any) {
			t.Error("should not happen")
		}),
	}

	pc := &PeerConnection{}

	err := FromStream(desc, r, pc)
	require.Equal(t, errNoSupportedCodecsFrom, err)
}

func TestFromStreamSkipUnsupportedTracks(t *testing.T) {
	desc := &description.Session{Medias: []*description.Media{
		{
			Type:    description.MediaTypeVideo,
			Formats: []format.Format{&format.H264{PacketizationMode: 1}},
		},
		{
			Type:    description.MediaTypeVideo,
			Formats: []format.Format{&format.MJPEG{}},
		},
	}}

	n := 0

	r := &stream.Reader{
		Parent: test.Logger(func(l logger.Level, format string, args ...any) {
			require.Equal(t, logger.Warn, l)
			if n == 0 {
				require.Equal(t, "skipping track 2 (M-JPEG)", fmt.Sprintf(format, args...))
			}
			n++
		}),
	}

	pc := &PeerConnection{}

	err := FromStream(desc, r, pc)
	require.NoError(t, err)

	require.Equal(t, 1, n)
}

// realSPS352x288 is a real, previously-verified H264 SPS NALU (see
// mediacommon's h264.SPS test fixtures) encoding a 352x288 picture -
// used here only to exercise SPS parsing, not to represent any of the
// resolutions mmx's own layerDefaults table guesses.
var realSPS352x288 = []byte{
	0x67, 0x64, 0x00, 0x0c, 0xac, 0x3b, 0x50, 0xb0,
	0x4b, 0x42, 0x00, 0x00, 0x03, 0x00, 0x02, 0x00,
	0x00, 0x03, 0x00, 0x3d, 0x08,
}

// minimalPPS is a placeholder PPS NALU (type 8): the stream package's
// unitRemuxerH264 strips SPS/PPS out of every access unit unless it
// already has *both* stored for that format (see
// internal/stream/unit_remuxer.go and format_updater.go, which store
// them from the first IDR that carries them) - without a PPS alongside
// realSPS352x288 above, the keyframe below would get its SPS silently
// dropped again by the time it reaches SetupFromStreamABR's filter,
// defeating this test. Its content is never parsed, only its NALU type.
var minimalPPS = []byte{0x68, 0x88, 0x84}

// TestSetupFromStreamABRResolvesDimensionsForNonActiveLayer reproduces a
// real-world report: publishing 1280x720 with 3 Simulcast layers made the
// player show wrong labels for every layer except the highest (e.g.
// "720p, 720p, 360p" instead of "720p, 480p, 240p" for OBS's actual
// 1280x720/852x480/426x240 split). Root cause: only the TrackSelector's
// initially-active layer (index 0) had its units reach the SPS-parsing
// code, because that code used to live in the OnDataFiltered callback,
// which ShouldQueue only lets through for the active/pending layer - see
// SetupFromStreamABR. This test writes a real SPS-bearing access unit to
// a *non-active* layer and checks its TrackInfo is corrected without ever
// selecting that layer.
func TestSetupFromStreamABRResolvesDimensionsForNonActiveLayer(t *testing.T) {
	desc := &description.Session{Medias: []*description.Media{
		{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PayloadTyp: 96, PacketizationMode: 1}}},
		{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PayloadTyp: 96, PacketizationMode: 1}}},
		{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PayloadTyp: 96, PacketizationMode: 1}}},
	}}

	strm := &stream.Stream{
		OrigDesc:          desc,
		WriteQueueSize:    512,
		RTPMaxPayloadSize: 1450,
		Parent:            test.NilLogger,
	}
	require.NoError(t, strm.Initialize())

	subStream := &stream.SubStream{Stream: strm}
	require.NoError(t, subStream.Initialize())

	selector := NewTrackSelector(test.NilLogger, func(int, int) {})
	require.NoError(t, selector.LoadFromDescription(desc))
	// layer 0 (index 0) is active by default - see TrackSelector.LoadFromDescription
	require.Equal(t, 0, selector.ActiveTrackID())

	r := &stream.Reader{Parent: test.NilLogger}
	err := SetupFromStreamABR(desc, r, &PeerConnection{}, selector)
	require.NoError(t, err)

	strm.AddReader(r)
	defer strm.RemoveReader(r)

	// write a real-SPS keyframe to layer 1, never selecting it
	subStream.WriteUnit(desc.Medias[1], desc.Medias[1].Formats[0], &unit.Unit{
		PTS: 0,
		NTP: time.Now(),
		Payload: unit.PayloadH264{
			realSPS352x288, minimalPPS,
			{0x65, 0x88, 0x84}, // minimal IDR slice
		},
	})

	require.Eventually(t, func() bool {
		tracks := selector.GetTracks()
		return tracks[1].Width == 352 && tracks[1].Height == 288
	}, 2*time.Second, 10*time.Millisecond,
		"layer 1's guessed dimensions must be replaced by its real SPS even though it was never selected")

	require.Equal(t, 0, selector.ActiveTrackID(), "writing to layer 1 must not have selected it")
}

func TestSetupFromStreamMultiH264(t *testing.T) {
	desc := &description.Session{Medias: []*description.Media{
		{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PacketizationMode: 1}}},
		{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PacketizationMode: 1}}},
		{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PacketizationMode: 1}}},
		{Type: description.MediaTypeAudio, Formats: []format.Format{&format.Opus{ChannelCount: 2}}},
		{Type: description.MediaTypeApplication, Formats: []format.Format{&format.KLV{PayloadTyp: 96}}},
	}}
	pc := &PeerConnection{}
	err := SetupFromStreamMultiH264(desc, &stream.Reader{Parent: test.NilLogger}, pc, 3)
	require.NoError(t, err)
	require.Len(t, pc.OutboundTracks, 4)
	require.Equal(t, []string{"video-0", "video-1", "video-2", "audio"}, []string{
		pc.OutboundTracks[0].TrackID,
		pc.OutboundTracks[1].TrackID,
		pc.OutboundTracks[2].TrackID,
		pc.OutboundTracks[3].TrackID,
	})
	require.Len(t, pc.OutboundDataChannels, 1)
	require.Equal(t, "KLV", pc.OutboundDataChannels[0].Label)

	// videoTrackCount is only an upper bound (see AutoVideoTracks): a
	// stream with fewer H264 layers than requested is served as-is
	// instead of rejected.
	pc2 := &PeerConnection{}
	err = SetupFromStreamMultiH264(&description.Session{Medias: desc.Medias[:2]},
		&stream.Reader{Parent: test.NilLogger}, pc2, 3)
	require.NoError(t, err)
	require.Len(t, pc2.OutboundTracks, 2)
	require.Equal(t, []string{"video-0", "video-1"}, []string{
		pc2.OutboundTracks[0].TrackID,
		pc2.OutboundTracks[1].TrackID,
	})

	err = SetupFromStreamMultiH264(&description.Session{Medias: []*description.Media{
		{Type: description.MediaTypeAudio, Formats: []format.Format{&format.Opus{ChannelCount: 2}}},
	}}, &stream.Reader{Parent: test.NilLogger}, &PeerConnection{}, 3)
	require.EqualError(t, err, "stream doesn't contain any H264 video layer")
}

func TestSetupFromStreamSimulcast(t *testing.T) {
	desc := &description.Session{Medias: []*description.Media{
		{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PacketizationMode: 1}}},
		{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PacketizationMode: 1}}},
		{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PacketizationMode: 1}}},
		{Type: description.MediaTypeAudio, Formats: []format.Format{&format.Opus{ChannelCount: 2}}},
		{Type: description.MediaTypeApplication, Formats: []format.Format{&format.KLV{PayloadTyp: 96}}},
	}}
	pc := &PeerConnection{}
	err := SetupFromStreamSimulcast(desc, &stream.Reader{Parent: test.NilLogger}, pc)
	require.NoError(t, err)
	require.Len(t, pc.OutboundTracks, 4)

	// the 3 video layers share one TrackID (one m-line) and each get their
	// own RID, highest quality first - see mmxForwardSimulcastTrackID.
	for i := range 3 {
		require.Equal(t, mmxForwardSimulcastTrackID, pc.OutboundTracks[i].TrackID)
		require.Equal(t, fmt.Sprint(i), pc.OutboundTracks[i].RID)
	}
	require.Equal(t, "audio", pc.OutboundTracks[3].TrackID)
	require.Empty(t, pc.OutboundTracks[3].RID)

	require.Len(t, pc.OutboundDataChannels, 1)
	require.Equal(t, "KLV", pc.OutboundDataChannels[0].Label)
}

func TestSetupFromStreamSimulcastNoVideo(t *testing.T) {
	desc := &description.Session{Medias: []*description.Media{
		{Type: description.MediaTypeAudio, Formats: []format.Format{&format.Opus{ChannelCount: 2}}},
	}}
	err := SetupFromStreamSimulcast(desc, &stream.Reader{Parent: test.NilLogger}, &PeerConnection{})
	require.Equal(t, errNoSupportedCodecsFrom, err)
}

// TestSetupFromStreamSimulcastOffer is the check that actually matters for
// mmx-to-mmx forwarding (see internal/forward): the produced offer must put
// every layer on a *single* video m-line as distinct RID encodings ("a=rid
// ... send" + "a=simulcast"), not one m-line per layer - otherwise the
// receiving mmx node's own WHIP publish endpoint rejects it outright (see
// TracksAreValid, called with maxVideoTracks=1 in
// internal/servers/webrtc/session.go).
func TestSetupFromStreamSimulcastOffer(t *testing.T) {
	desc := &description.Session{Medias: []*description.Media{
		{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PacketizationMode: 1}}},
		{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PacketizationMode: 1}}},
		{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PacketizationMode: 1}}},
		{Type: description.MediaTypeAudio, Formats: []format.Format{&format.Opus{ChannelCount: 2}}},
	}}

	pc := &PeerConnection{
		LocalRandomUDP:    true,
		IPsFromInterfaces: true,
		Publish:           true,
		Log:               test.NilLogger,
	}

	err := SetupFromStreamSimulcast(desc, &stream.Reader{Parent: test.NilLogger}, pc)
	require.NoError(t, err)

	err = pc.Start()
	require.NoError(t, err)
	defer pc.Close()

	offer, err := pc.CreateFullOffer()
	require.NoError(t, err)

	var sdpDesc sdp.SessionDescription
	require.NoError(t, sdpDesc.Unmarshal([]byte(offer.SDP)))

	videoMedias := 0
	var ridAttrs []string
	var simulcastAttr string
	for _, media := range sdpDesc.MediaDescriptions {
		if media.MediaName.Media != "video" {
			continue
		}
		videoMedias++
		for _, attr := range media.Attributes {
			if attr.Key == "rid" {
				ridAttrs = append(ridAttrs, attr.Value)
			}
			if attr.Key == "simulcast" {
				simulcastAttr = attr.Value
			}
		}
	}

	require.Equal(t, 1, videoMedias, "all 3 layers must share a single video m-line")
	require.Equal(t, []string{"0 send", "1 send", "2 send"}, ridAttrs)
	require.Equal(t, "send 0;1;2", simulcastAttr)

	// this is exactly the check the receiving mmx node's own WHIP publish
	// endpoint runs (session.go: TracksAreValid(medias, 1, 0)) - it must
	// see only 1 video track despite the 3 Simulcast layers.
	require.NoError(t, TracksAreValid(sdpDesc.MediaDescriptions, 1, 0))
}

// TestSetupFromStreamSimulcastRealConnection is an end-to-end check (real
// PeerConnections, real RTP over loopback) that a mmx-forwarded Simulcast
// offer is not just SDP-shaped correctly (see
// TestSetupFromStreamSimulcastOffer) but actually gets demuxed into 3
// distinct InboundTracks on the receiving side, one per RID, each
// receiving the packets written to its own OutboundTrack. This is the
// scenario that silently failed before OutboundTrack stamped mid/rid RTP
// header extensions on every packet (see resolveSimulcastExtIDs): the
// offer/answer looked correct, but the receiver's demuxer had no way to
// route incoming packets to the right InboundTrack and every layer after
// the first just timed out in GatherInboundTracks.
func TestSetupFromStreamSimulcastRealConnection(t *testing.T) {
	desc := &description.Session{Medias: []*description.Media{
		{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PacketizationMode: 1}}},
		{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PacketizationMode: 1}}},
		{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PacketizationMode: 1}}},
	}}

	publisher := &PeerConnection{
		LocalRandomUDP:    true,
		IPsFromInterfaces: true,
		Publish:           true,
		Log:               test.NilLogger,
	}

	r := &stream.Reader{Parent: test.NilLogger}
	err := SetupFromStreamSimulcast(desc, r, publisher)
	require.NoError(t, err)

	err = publisher.Start()
	require.NoError(t, err)
	defer publisher.Close()

	receiver := &PeerConnection{
		LocalRandomUDP:      true,
		IPsFromInterfaces:   true,
		Publish:             false,
		RecvOnlyVideoTracks: 3,
		Log:                 test.NilLogger,
	}
	err = receiver.Start()
	require.NoError(t, err)
	defer receiver.Close()

	offer, err := publisher.CreateFullOffer()
	require.NoError(t, err)

	answer, err := receiver.CreateFullAnswer(offer, false)
	require.NoError(t, err)

	err = publisher.SetAnswer(answer)
	require.NoError(t, err)

	err = publisher.WaitUntilConnected(10 * time.Second)
	require.NoError(t, err)
	err = receiver.WaitUntilConnected(10 * time.Second)
	require.NoError(t, err)

	// send a handful of RTP packets on each of the 3 OutboundTracks -
	// resolveSimulcastExtIDs only becomes resolvable once the answer's
	// mid is set, i.e. after CreateFullAnswer/SetAnswer above.
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for i := range uint16(50) {
			for _, tr := range publisher.OutboundTracks {
				tr.WriteRTP(&rtp.Packet{ //nolint:errcheck
					Header: rtp.Header{
						Version:        2,
						SequenceNumber: i,
						Timestamp:      uint32(i) * 3000,
					},
					Payload: []byte{0x65, 0x88, 0x84},
				})
			}
			<-ticker.C
		}
	}()

	err = receiver.GatherInboundTracks(5 * time.Second)
	require.NoError(t, err)
	<-done

	// PeerConnection.Start auto-adds a PCMU OutboundTrack/RecvOnly audio
	// transceiver when a Publish side has no audio (see peer_connection.go)
	// - only the video (RID-bearing) tracks matter here.
	var videoRIDs []string
	for _, tr := range receiver.InboundTracks() {
		if tr.rid != "" {
			videoRIDs = append(videoRIDs, tr.rid)
		}
	}
	require.ElementsMatch(t, []string{"0", "1", "2"}, videoRIDs,
		"all 3 Simulcast layers must be demuxed into distinct InboundTracks")
}

func TestFromStream(t *testing.T) {
	for _, ca := range toFromStreamCases {
		t.Run(ca.name, func(t *testing.T) {
			desc := &description.Session{
				Medias: []*description.Media{{
					Formats: []format.Format{ca.in},
				}},
			}

			pc := &PeerConnection{}
			r := &stream.Reader{Parent: test.NilLogger}

			err := FromStream(desc, r, pc)
			require.NoError(t, err)

			expectedCaps := ca.webrtcCaps
			if ca.name == "h264" {
				// toFromStreamCases' fmtp line here is only used by TestToStream
				// (inbound codec parsing test input, arbitrary) - FromStream's
				// actual outbound fmtp is the fixed h264OutboundFmtpLine (see
				// from_stream.go), independent of the inbound stream's real SPS.
				expectedCaps.SDPFmtpLine = h264OutboundFmtpLine
			}
			require.Equal(t, expectedCaps, pc.OutboundTracks[0].Caps)
		})
	}
}

func TestFromStreamResampleOpus(t *testing.T) {
	strm := &stream.Stream{
		OrigDesc: &description.Session{Medias: []*description.Media{
			{
				Type: description.MediaTypeAudio,
				Formats: []format.Format{&format.Opus{
					ChannelCount: 2,
				}},
			},
		}},
		WriteQueueSize:    512,
		RTPMaxPayloadSize: 1450,
		ReplaceNTP:        false,
		Parent:            test.NilLogger,
	}
	err := strm.Initialize()
	require.NoError(t, err)

	subStream := &stream.SubStream{
		Stream:        strm,
		UseRTPPackets: true,
	}
	err = subStream.Initialize()
	require.NoError(t, err)

	pc1 := &PeerConnection{
		LocalRandomUDP:    true,
		IPsFromInterfaces: true,
		Publish:           false,
		Log:               test.NilLogger,
	}
	err = pc1.Start()
	require.NoError(t, err)
	defer pc1.Close()

	pc2 := &PeerConnection{
		LocalRandomUDP:    true,
		IPsFromInterfaces: true,
		Publish:           true,
		Log:               test.NilLogger,
	}

	r := &stream.Reader{Parent: nil}

	err = FromStream(strm.OrigDesc, r, pc2)
	require.NoError(t, err)

	err = pc2.Start()
	require.NoError(t, err)
	defer pc2.Close()

	offer, err := pc1.CreatePartialOffer(false)
	require.NoError(t, err)

	answer, err := pc2.CreateFullAnswer(offer, false)
	require.NoError(t, err)

	err = pc1.SetAnswer(answer)
	require.NoError(t, err)

	err = pc1.WaitUntilConnected(10 * time.Second)
	require.NoError(t, err)

	err = pc2.WaitUntilConnected(10 * time.Second)
	require.NoError(t, err)

	strm.AddReader(r)
	defer strm.RemoveReader(r)

	subStream.WriteUnit(strm.OrigDesc.Medias[0], strm.OrigDesc.Medias[0].Formats[0], &unit.Unit{
		PTS: 0,
		NTP: time.Now(),
		RTPPackets: []*rtp.Packet{{
			Header: rtp.Header{
				Version:        2,
				Marker:         true,
				PayloadType:    111,
				SequenceNumber: 1123,
				Timestamp:      45343,
				SSRC:           563424,
			},
			Payload: []byte{1},
		}},
	})

	subStream.WriteUnit(strm.OrigDesc.Medias[0], strm.OrigDesc.Medias[0].Formats[0], &unit.Unit{
		PTS: 0,
		NTP: time.Now(),
		RTPPackets: []*rtp.Packet{{
			Header: rtp.Header{
				Version:        2,
				Marker:         true,
				PayloadType:    111,
				SequenceNumber: 1124,
				Timestamp:      45343,
				SSRC:           563424,
			},
			Payload: []byte{1},
		}},
	})

	err = pc1.GatherInboundTracks(2 * time.Second)
	require.NoError(t, err)

	tracks := pc1.InboundTracks()

	done := make(chan struct{})
	n := 0
	var ts uint32

	tracks[0].OnPacketRTP = func(pkt *rtp.Packet) {
		n++

		switch n {
		case 1:
			ts = pkt.Timestamp

		case 2:
			require.Equal(t, uint32(960), pkt.Timestamp-ts)
			close(done)
		}
	}

	pc1.StartReading()

	<-done
}

func TestFromStreamResampleOpusAbsoluteTimestamp(t *testing.T) {
	strm := &stream.Stream{
		OrigDesc: &description.Session{Medias: []*description.Media{
			{
				Type: description.MediaTypeAudio,
				Formats: []format.Format{&format.Opus{
					ChannelCount: 2,
				}},
			},
		}},
		WriteQueueSize:    512,
		RTPMaxPayloadSize: 1450,
		ReplaceNTP:        false,
		Parent:            test.NilLogger,
	}
	err := strm.Initialize()
	require.NoError(t, err)

	subStream := &stream.SubStream{
		Stream:        strm,
		UseRTPPackets: true,
	}
	err = subStream.Initialize()
	require.NoError(t, err)

	pcReader := &PeerConnection{
		LocalRandomUDP:    true,
		IPsFromInterfaces: true,
		Publish:           false,
		Log:               test.NilLogger,
	}
	err = pcReader.Start()
	require.NoError(t, err)
	t.Cleanup(pcReader.Close)

	pcPublisher := &PeerConnection{
		LocalRandomUDP:    true,
		IPsFromInterfaces: true,
		Publish:           true,
		Log:               test.NilLogger,
	}

	r := &stream.Reader{Parent: nil}

	err = FromStream(strm.OrigDesc, r, pcPublisher)
	require.NoError(t, err)

	err = pcPublisher.Start()
	require.NoError(t, err)
	t.Cleanup(pcPublisher.Close)

	offer, err := pcReader.CreatePartialOffer(false)
	require.NoError(t, err)

	answer, err := pcPublisher.CreateFullAnswer(offer, false)
	require.NoError(t, err)

	err = pcReader.SetAnswer(answer)
	require.NoError(t, err)

	err = pcReader.WaitUntilConnected(10 * time.Second)
	require.NoError(t, err)

	err = pcPublisher.WaitUntilConnected(10 * time.Second)
	require.NoError(t, err)

	strm.AddReader(r)
	t.Cleanup(func() { strm.RemoveReader(r) })

	baseNTP := time.Unix(1710000000, 0)
	step := 20 * time.Millisecond

	// prime the pipeline to allow track gathering
	subStream.WriteUnit(strm.OrigDesc.Medias[0], strm.OrigDesc.Medias[0].Formats[0], &unit.Unit{
		PTS: 0,
		NTP: baseNTP,
		RTPPackets: []*rtp.Packet{{
			Header: rtp.Header{
				Version:        2,
				Marker:         true,
				PayloadType:    111,
				SequenceNumber: 1123,
				Timestamp:      45343,
				SSRC:           563424,
			},
			Payload: []byte{1},
		}},
	})

	err = pcReader.GatherInboundTracks(2 * time.Second)
	require.NoError(t, err)

	tracks := pcReader.InboundTracks()
	require.Len(t, tracks, 1)

	done := make(chan struct{})
	errCh := make(chan string, 1)
	const startSeq = uint16(2000)

	expectedNTP := func(seq uint16) (time.Time, bool) {
		if seq < startSeq {
			return time.Time{}, false
		}
		return baseNTP.Add(time.Duration(seq-startSeq) * step), true
	}

	tracks[0].OnPacketRTP = func(pkt *rtp.Packet) {
		expected, ok := expectedNTP(pkt.SequenceNumber)
		if !ok {
			return
		}

		ntp, avail := tracks[0].PacketNTP(pkt)
		if !avail {
			return
		}

		if ntp.Sub(expected).Abs() > 50*time.Millisecond {
			select {
			case errCh <- fmt.Sprintf("absolute NTP mismatch for seq=%d: got=%v expected=%v",
				pkt.SequenceNumber, ntp, expected):
			default:
			}
			return
		}

		select {
		case done <- struct{}{}:
		default:
		}
	}

	pcReader.StartReading()

	go func() {
		ticker := time.NewTicker(step)
		defer ticker.Stop()

		for i := range uint16(150) {
			seq := startSeq + i
			expected, _ := expectedNTP(seq)

			subStream.WriteUnit(strm.OrigDesc.Medias[0], strm.OrigDesc.Medias[0].Formats[0], &unit.Unit{
				PTS: 0,
				NTP: expected,
				RTPPackets: []*rtp.Packet{{
					Header: rtp.Header{
						Version:        2,
						Marker:         true,
						PayloadType:    111,
						SequenceNumber: seq,
						Timestamp:      45343,
						SSRC:           563424,
					},
					Payload: []byte{1},
				}},
			})

			<-ticker.C
		}
	}()

	select {
	case <-done:
	case err := <-errCh:
		t.Fatal(err)
	case <-time.After(8 * time.Second):
		t.Fatal("absolute timestamp mapping did not become available")
	}
}
