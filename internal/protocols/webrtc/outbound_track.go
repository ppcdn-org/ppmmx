package webrtc

import (
	"fmt"
	"strings"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/rtpsender"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"github.com/bluenviron/mediamtx/internal/logger"
)

// OutboundTrack is an outgoing track. Setting RID turns it into one
// Simulcast encoding of a shared sender: every OutboundTrack that shares
// the same TrackID and has RID set is sent on a single video m-line with
// multiple "a=rid ... send" (RFC 8853 Simulcast, the same SDP shape OBS's
// own WHIP Simulcast offer uses - see internal/servers/webrtc/session.go's
// offerVideoSimulcastLayerCount) instead of one m-line per track. This is
// what lets mmx forward its own multi-layer stream to another mmx node's
// WHIP publish endpoint, which only ever accepts a single video m-line
// (see TracksAreValid).
type OutboundTrack struct {
	Caps    webrtc.RTPCodecCapability
	TrackID string
	// RID marks this OutboundTrack as one Simulcast encoding rather than
	// its own independent m-line - see the type doc above. Leave empty for
	// the normal (WHEP playback, Tencent forwarding) single-encoding case.
	RID string

	track      *webrtc.TrackLocalStaticRTP
	ssrc       uint32
	rtcpSender *rtpsender.Sender
	log        logger.Writer

	// simulcastExtIDs, once resolved, are stamped on every outgoing packet
	// of a Simulcast encoding (RID != "") - see WriteRTPWithNTP. Without
	// them the remote peer's demuxer has no way to tell which RID a packet
	// belongs to (pion's own TrackLocalStaticRTP.WriteRTP does not add
	// these on its own, see RFC 8853 §4 and pion's own Simulcast-sending
	// tests, which all set these by hand) and every layer after the first
	// never gets dispatched to an OnTrack callback on the receiving end.
	simulcastExtIDs *simulcastExtIDs
	sender          *webrtc.RTPSender
	pc              *PeerConnection
}

// simulcastExtIDs are the per-connection RTP header extension IDs (mid,
// rid) needed to stamp a Simulcast-encoding packet - see WriteRTPWithNTP.
type simulcastExtIDs struct {
	mid   string
	midID uint8
	ridID uint8
}

func (t *OutboundTrack) isVideo() bool {
	return strings.Split(t.Caps.MimeType, "/")[0] == "video"
}

// setup binds this OutboundTrack as a brand new WebRTC track/transceiver
// (RID unset or the group's first encoding), returning the sender so the
// caller can pass it to later encodings' addEncodingTo - see
// PeerConnection.Start.
func (t *OutboundTrack) setup(p *PeerConnection) (*webrtc.RTPSender, error) {
	t.log = p.Log
	trackID := t.TrackID
	if trackID == "" {
		if t.isVideo() {
			trackID = "video"
		} else {
			trackID = "audio"
		}
	}

	var err error
	t.track, err = webrtc.NewTrackLocalStaticRTP(
		t.Caps,
		trackID,
		webrtcStreamID,
		webrtc.WithRTPStreamID(t.RID),
	)
	if err != nil {
		return nil, err
	}

	sender, err := p.wr.AddTrack(t.track)
	if err != nil {
		return nil, err
	}

	err = t.bindSSRC(p, sender, t.RID)
	if err != nil {
		return nil, err
	}

	t.pc = p
	t.sender = sender

	return sender, nil
}

// addEncodingTo binds this OutboundTrack as an additional Simulcast
// encoding of an already-added sender (base is the group's first
// OutboundTrack, already setup()). Both must share TrackID/StreamID/Kind -
// see RTPSender.AddEncoding - which webrtcStreamID and the shared trackID
// computed in setup already guarantee.
func (t *OutboundTrack) addEncodingTo(p *PeerConnection, base *OutboundTrack, sender *webrtc.RTPSender) error {
	t.log = p.Log

	var err error
	t.track, err = webrtc.NewTrackLocalStaticRTP(
		t.Caps,
		base.track.ID(),
		base.track.StreamID(),
		webrtc.WithRTPStreamID(t.RID),
	)
	if err != nil {
		return err
	}

	err = sender.AddEncoding(t.track)
	if err != nil {
		return err
	}

	t.pc = p
	t.sender = sender

	return t.bindSSRC(p, sender, t.RID)
}

// bindSSRC looks up the SSRC pion assigned to this encoding's RID (empty
// RID means the sender's only/base encoding) and starts this
// OutboundTrack's own RTCP sender-report generator for it - each Simulcast
// encoding has its own SSRC and needs its own sender reports.
func (t *OutboundTrack) bindSSRC(p *PeerConnection, sender *webrtc.RTPSender, rid string) error {
	for _, encoding := range sender.GetParameters().Encodings {
		if encoding.RID == rid {
			t.ssrc = uint32(encoding.SSRC)
			break
		}
	}

	t.rtcpSender = &rtpsender.Sender{
		ClockRate: int(t.track.Codec().ClockRate),
		Period:    1 * time.Second,
		TimeNow:   time.Now,
		WritePacketRTCP: func(pkt rtcp.Packet) {
			p.wr.WriteRTCP([]rtcp.Packet{pkt}) //nolint:errcheck
		},
	}
	t.rtcpSender.Initialize()

	return nil
}

// drainSenderRTCP must be called once per sender (not once per RID
// encoding) to keep interceptors working - see PeerConnection.Start.
func drainSenderRTCP(sender *webrtc.RTPSender) {
	go func() {
		buf := make([]byte, 1500)
		for {
			n, _, err := sender.Read(buf)
			if err != nil {
				return
			}

			_, err = rtcp.Unmarshal(buf[:n])
			if err != nil {
				panic(err)
			}
		}
	}()
}

func (t *OutboundTrack) close() {
	if t.rtcpSender != nil {
		t.rtcpSender.Close()
	}
}

// resolveSimulcastExtIDs resolves and caches the mid/rid RTP header
// extension IDs and this sender's negotiated mid value. It's called lazily
// from WriteRTPWithNTP (not from setup/addEncodingTo) because the mid is
// only assigned once CreateFullOffer/CreateFullAnswer runs SetLocalDescription
// (see pion's PeerConnection.SetLocalDescription -> RTPTransceiver.SetMid),
// which happens after PeerConnection.Start has already set up every
// OutboundTrack.
func (t *OutboundTrack) resolveSimulcastExtIDs() *simulcastExtIDs {
	if t.simulcastExtIDs != nil {
		return t.simulcastExtIDs
	}

	ids := &simulcastExtIDs{}
	for _, ext := range t.sender.GetParameters().HeaderExtensions {
		switch ext.URI {
		case sdp.SDESMidURI:
			ids.midID = uint8(ext.ID) //nolint:gosec
		case sdp.SDESRTPStreamIDURI:
			ids.ridID = uint8(ext.ID) //nolint:gosec
		}
	}

	for _, tr := range t.pc.wr.GetTransceivers() {
		if tr.Sender() == t.sender {
			ids.mid = tr.Mid()
			break
		}
	}

	if ids.mid == "" || ids.midID == 0 || ids.ridID == 0 {
		return nil
	}

	t.simulcastExtIDs = ids
	return ids
}

// WriteRTP writes a RTP packet.
func (t *OutboundTrack) WriteRTP(pkt *rtp.Packet) error {
	return t.WriteRTPWithNTP(pkt, time.Now())
}

// WriteRTPWithNTP writes a RTP packet.
func (t *OutboundTrack) WriteRTPWithNTP(pkt *rtp.Packet, ntp time.Time) error {
	// use right SSRC in packet to make rtcpSender work
	pkt.SSRC = t.ssrc

	// Simulcast encodings must carry mid/rid RTP header extensions on
	// every packet so the remote peer's demuxer can tell them apart - see
	// OutboundTrack's simulcastExtIDs field doc. SetExtension sets
	// pkt.Extension/ExtensionProfile itself on the first call for a
	// packet with none yet - setting pkt.Extension=true beforehand would
	// leave ExtensionProfile at its zero value, which SetExtension reads
	// as the (single, ID-0-only) RFC3550 profile and rejects our IDs.
	if t.RID != "" {
		if ids := t.resolveSimulcastExtIDs(); ids != nil {
			pkt.SetExtension(ids.midID, []byte(ids.mid)) //nolint:errcheck
			pkt.SetExtension(ids.ridID, []byte(t.RID))   //nolint:errcheck
		}
	}

	t.rtcpSender.ProcessPacket(pkt, ntp, true)

	err := t.track.WriteRTP(pkt)
	if err != nil && t.log != nil {
		t.log.Log(logger.Warn,
			"RTP send failed: media=%s ssrc=%d pt=%d seq=%d timestamp=%d marker=%t err=%v",
			strings.Split(t.Caps.MimeType, "/")[0], pkt.SSRC, pkt.PayloadType,
			pkt.SequenceNumber, pkt.Timestamp, pkt.Marker, err)
		return fmt.Errorf("write RTP: %w", err)
	}
	return err
}
