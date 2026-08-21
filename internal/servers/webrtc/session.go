package webrtc

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/google/uuid"
	"github.com/pion/ice/v4"
	"github.com/pion/sdp/v3"
	"github.com/pion/transport/v4"
	pwebrtc "github.com/pion/webrtc/v4"

	"github.com/bluenviron/mediamtx/internal/auth"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/externalcmd"
	"github.com/bluenviron/mediamtx/internal/hooks"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/protocols/httpp"
	"github.com/bluenviron/mediamtx/internal/protocols/webrtc"
	wsproto "github.com/bluenviron/mediamtx/internal/protocols/websocket"
	"github.com/bluenviron/mediamtx/internal/protocols/whip"
	"github.com/bluenviron/mediamtx/internal/stream"
)

func whipOffer(body []byte) *pwebrtc.SessionDescription {
	return &pwebrtc.SessionDescription{
		Type: pwebrtc.SDPTypeOffer,
		SDP:  string(body),
	}
}

func offerH264SendTrackCount(medias []*sdp.MediaDescription) (int, error) {
	count := 0
	for _, media := range medias {
		if media.MediaName.Media != "video" || media.MediaName.Port.Value == 0 {
			continue
		}

		canSend := true
		for _, attr := range media.Attributes {
			if attr.Key == "sendonly" || attr.Key == "inactive" {
				canSend = false
				break
			}
		}
		if !canSend {
			continue
		}

		hasH264 := false
		for _, attr := range media.Attributes {
			if attr.Key != "rtpmap" {
				continue
			}
			fields := strings.Fields(attr.Value)
			if len(fields) == 2 && strings.EqualFold(strings.SplitN(fields[1], "/", 2)[0], "H264") {
				hasH264 = true
				break
			}
		}
		if hasH264 {
			count++
			if count > 4 {
				return 0, fmt.Errorf("multi-layer WHEP supports at most 4 H264 video tracks")
			}
		}
	}
	return count, nil
}

// maxWHIPSimulcastLayers is mmx's own cap on the number of RIDs (Simulcast
// layers) a WHIP publisher may offer on its single video m-line. OBS (the
// publisher) decides the actual layer count entirely on its own - this is
// not a mmx policy preference, only a hard ceiling: any offer requesting
// more than this many layers is rejected outright (406, see
// offerVideoSimulcastLayerCount's call site) rather than silently
// truncated, so the publisher gets an explicit failure and can retry with
// a layer count mmx will accept.
const maxWHIPSimulcastLayers = 5

// offerVideoSimulcastLayerCount returns how many Simulcast layers a WHIP
// publish offer requests: OBS puts every layer on the same (single) video
// m-line as one "a=rid:<n> send" attribute each (see
// frontend/utility/WHIPSimulcastEncoders.hpp and whip-output.cpp in the OBS
// repo) rather than one m-line per layer, so this counts rid attributes,
// not video m-lines. A video m-line with no rid attributes at all counts
// as a single (non-Simulcast) layer.
func offerVideoSimulcastLayerCount(medias []*sdp.MediaDescription) int {
	count := 0
	for _, media := range medias {
		if media.MediaName.Media != "video" {
			continue
		}

		ridCount := 0
		for _, attr := range media.Attributes {
			if attr.Key == "rid" {
				ridCount++
			}
		}
		if ridCount == 0 {
			ridCount = 1
		}
		count += ridCount
	}
	return count
}

func offerSendVideoTrackCount(medias []*sdp.MediaDescription) int {
	count := 0
	for _, media := range medias {
		if media.MediaName.Media != "video" || media.MediaName.Port.Value == 0 {
			continue
		}
		canSend := true
		for _, attr := range media.Attributes {
			if attr.Key == "sendonly" || attr.Key == "inactive" {
				canSend = false
				break
			}
		}
		if canSend {
			count++
		}
	}
	return count
}

func parseOfferUfrag(offer []byte) string {
	var desc sdp.SessionDescription
	if err := desc.Unmarshal(offer); err != nil {
		return ""
	}

	// per-media credentials (priority matches sdpFragmentToCredentials)
	for _, media := range desc.MediaDescriptions {
		if ufrag, ok := media.Attribute("ice-ufrag"); ok && ufrag != "" {
			return ufrag
		}
	}

	// session-level credentials
	for _, attr := range desc.Attributes {
		if attr.Key == "ice-ufrag" && attr.Value != "" {
			return attr.Value
		}
	}
	return ""
}

func replaceICECredentials(offerSDP []byte, ufrag, pwd string) []byte {
	s := string(offerSDP)
	sep := "\r\n"
	if !strings.Contains(s, "\r\n") {
		sep = "\n"
	}
	lines := strings.Split(s, sep)
	for i, line := range lines {
		if strings.HasPrefix(line, "a=ice-ufrag:") {
			lines[i] = "a=ice-ufrag:" + ufrag
		} else if strings.HasPrefix(line, "a=ice-pwd:") {
			lines[i] = "a=ice-pwd:" + pwd
		}
	}
	return []byte(strings.Join(lines, sep))
}

func sdpFragmentToCredentials(frag *whip.SDPFragment) (string, string, error) {
	// media credentials
	for _, media := range frag.Medias {
		ufrag, _ := media.Attribute("ice-ufrag")
		pwd, _ := media.Attribute("ice-pwd")
		if ufrag != "" && pwd != "" {
			return ufrag, pwd, nil
		}
	}

	// session-wide credentials
	var ufrag, pwd string
	for _, attr := range frag.Attributes {
		switch attr.Key {
		case "ice-ufrag":
			ufrag = attr.Value
		case "ice-pwd":
			pwd = attr.Value
		}
	}
	if ufrag != "" && pwd != "" {
		return ufrag, pwd, nil
	}

	return "", "", fmt.Errorf("ICE credentials not found")
}

func sdpFragmentToCandidates(frag *whip.SDPFragment) ([]*pwebrtc.ICECandidateInit, error) {
	var candidates []*pwebrtc.ICECandidateInit

	for _, media := range frag.Medias {
		mid, ok := media.Attribute("mid")
		if !ok {
			return nil, fmt.Errorf("mid attribute is missing")
		}

		tmp, err := strconv.ParseUint(mid, 10, 16)
		if err != nil {
			return nil, fmt.Errorf("invalid mid attribute")
		}
		midNum := uint16(tmp)

		for _, attr := range media.Attributes {
			if attr.Key == "candidate" {
				candidates = append(candidates, &pwebrtc.ICECandidateInit{
					Candidate:     attr.Value,
					SDPMid:        &mid,
					SDPMLineIndex: &midNum,
				})
			}
		}
	}

	return candidates, nil
}

func mediaHasCredentialsOrCandidates(media *sdp.MediaDescription) bool {
	hasUfrag := false
	hasPwd := false

	for _, attr := range media.Attributes {
		if attr.Value != "" {
			switch attr.Key {
			case "ice-ufrag":
				hasUfrag = true

			case "ice-pwd":
				hasPwd = true

			case "candidate":
				return true
			}
		}
	}

	return (hasUfrag && hasPwd)
}

func fullAnswerToSDPFragment(answerSDP string) (*whip.SDPFragment, error) {
	var psdp sdp.SessionDescription
	err := psdp.Unmarshal([]byte(answerSDP))
	if err != nil {
		return nil, err
	}

	frag := &whip.SDPFragment{
		Attributes: []sdp.Attribute{
			{Key: "ice-options", Value: "trickle ice2"},
		},
	}

	filled := false

	for _, attr := range psdp.Attributes {
		switch attr.Key {
		case "ice-ufrag", "ice-pwd":
			frag.Attributes = append(frag.Attributes, sdp.Attribute{Key: attr.Key, Value: attr.Value})
			filled = true
		}
	}

	for _, media := range psdp.MediaDescriptions {
		if mediaHasCredentialsOrCandidates(media) {
			filled = true

			mid, ok := media.Attribute("mid")
			if !ok {
				return nil, fmt.Errorf("mid attribute is missing")
			}

			mediaFrag := &sdp.MediaDescription{
				MediaName: media.MediaName,
				Attributes: []sdp.Attribute{
					{Key: "mid", Value: mid},
				},
			}

			ufrag, _ := media.Attribute("ice-ufrag")
			pwd, _ := media.Attribute("ice-pwd")
			if ufrag != "" && pwd != "" {
				mediaFrag.Attributes = append(mediaFrag.Attributes, sdp.Attribute{Key: "ice-ufrag", Value: ufrag})
				mediaFrag.Attributes = append(mediaFrag.Attributes, sdp.Attribute{Key: "ice-pwd", Value: pwd})
			}

			for _, attr := range media.Attributes {
				if attr.Key == "candidate" {
					mediaFrag.Attributes = append(mediaFrag.Attributes, attr)
				}
			}
			mediaFrag.Attributes = append(mediaFrag.Attributes, sdp.Attribute{Key: "end-of-candidates"})

			frag.Medias = append(frag.Medias, mediaFrag)
		}
	}

	if !filled {
		return nil, fmt.Errorf("no credentials or candidates found in the answer")
	}

	return frag, nil
}

type initialRequestRes struct {
	answer        []byte
	err           error
	errStatusCode int
}

type initialRequestReq struct {
	res chan initialRequestRes
}

type sessionParent interface {
	closeSession(sx *session)
	generateICEServers(clientConfig bool) ([]pwebrtc.ICEServer, error)
	logger.Writer

	// WHIP degrade protocol (see docs/obs-mmx-degrade-protocol.md)
	degradeSampleEnabled() bool
	recordDegradeSample(pathName string, cumLost, cumReceived uint64)
	observeDegradeSessionLayers(pathName string, realLayers int)

	// WHIP publish reconnect tracking (see publishstats.go) - independent
	// of the degrade protocol, gives operators plain visibility into how
	// often a publisher has reconnected during a streaming period.
	recordPublishSessionStart(pathName string) publishSessionStartInfo
	recordPublishSessionEnd(pathName string)
	publishStatsSummary(pathName string) (time.Duration, int)

	// OBS abs-timestamp fan-out (see obs_timestamp_broadcast.go): relays
	// "obs-timestamp" DataChannel messages from a path's WHIP publish
	// session to all of that path's WHEP reader sessions over their ABR
	// control WebSocket, so a player can compute true end-to-end p2p
	// delay (see docs/obs-abs-timestamp-protocol.md in the OBS repo).
	subscribeObsTimestamp(pathName string, sx *session)
	unsubscribeObsTimestamp(pathName string, sx *session)
	broadcastObsTimestamp(pathName string, msg abrMessage)
}

type session struct {
	net                   transport.Net
	parentCtx             context.Context
	ipsFromInterfaces     bool
	ipsFromInterfacesList []string
	additionalHosts       []string
	iceUDPMux             ice.UDPMux
	iceTCPMux             *webrtc.TCPMuxWrapper
	stunGatherTimeout     conf.Duration
	handshakeTimeout      conf.Duration
	trackGatherTimeout    conf.Duration
	pathName              string
	remoteAddr            string
	offer                 []byte
	publish               bool
	httpRequest           *http.Request
	wg                    *sync.WaitGroup
	externalCmdPool       *externalcmd.Pool
	pathManager           serverPathManager
	parent                sessionParent

	ctx       context.Context
	ctxCancel func()
	created   time.Time
	uuid      uuid.UUID
	secret    uuid.UUID
	mutex     sync.RWMutex
	reader    *stream.Reader
	pc        *webrtc.PeerConnection
	user      string

	chInitialRequest chan initialRequestReq
	chAddCandidates  chan addSessionCandidatesReq

	// ABR (Adaptive Bitrate)
	abrEnabled        bool
	trackSelector     *webrtc.TrackSelector
	wsConn            *wsproto.ServerConn
	wsWriteMutex      sync.Mutex
	lastSwitchTime    time.Time
	abrSwitchCooldown int // ms
	mediaState        *webrtc.MediaState
	abrReady          bool

	// OBS abs-timestamp protocol (see docs/obs-abs-timestamp-protocol.md
	// in the OBS repo): end-to-end publish latency computed from the most
	// recently received "obs-timestamp" DataChannel message, in
	// milliseconds. nil until the first message arrives (e.g. the
	// publisher isn't a patched OBS build, or hasn't sent one yet).
	obsTimestampLatencyMs *float64
}

func (s *session) writeABRMessage(msg abrMessage) error {
	s.wsWriteMutex.Lock()
	defer s.wsWriteMutex.Unlock()
	s.mutex.RLock()
	ws := s.wsConn
	s.mutex.RUnlock()
	if ws == nil {
		return nil
	}
	return ws.WriteJSON(msg)
}

func (s *session) initialize() {
	s.ctx, s.ctxCancel = context.WithCancel(s.parentCtx)
	s.created = time.Now()
	s.uuid = uuid.New()
	s.secret = uuid.New()
	s.chInitialRequest = make(chan initialRequestReq)
	s.chAddCandidates = make(chan addSessionCandidatesReq)

	s.Log(logger.Info, "created by %s", s.remoteAddr)

	s.wg.Add(1)

	go s.run()
}

// Log implements logger.Writer.
func (s *session) Log(level logger.Level, format string, args ...any) {
	id := hex.EncodeToString(s.uuid[:4])
	s.parent.Log(level, "[session %v] "+format, append([]any{id}, args...)...)
}

func (s *session) Close() {
	s.ctxCancel()
	// Close WebSocket to unblock any pending ReadJSON in ABR handler
	s.mutex.RLock()
	ws := s.wsConn
	s.mutex.RUnlock()
	if ws != nil {
		ws.Close()
	}
}

func (s *session) run() {
	defer s.wg.Done()

	err := s.runInner()

	s.ctxCancel()

	s.parent.closeSession(s)

	s.Log(logger.Info, "closed: %v", err)
}

func (s *session) runInner() error {
	var req initialRequestReq
	select {
	case req = <-s.chInitialRequest:
	case <-s.ctx.Done():
		return fmt.Errorf("terminated")
	}

	errStatusCode, err := s.runInner2(&req)

	if errStatusCode != 0 {
		req.res <- initialRequestRes{
			errStatusCode: errStatusCode,
			err:           err,
		}
	}

	return err
}

func (s *session) runInner2(req *initialRequestReq) (int, error) {
	if s.publish {
		return s.runPublish(req)
	}
	return s.runRead(req)
}

func (s *session) runPublish(req *initialRequestReq) (int, error) {
	ip, _, _ := net.SplitHostPort(s.remoteAddr)

	res1, err := s.pathManager.FindPathConf(defs.PathFindPathConfReq{
		AccessRequest: defs.PathAccessRequest{
			Name:        s.pathName,
			Query:       s.httpRequest.URL.RawQuery,
			Publish:     true,
			UserAgent:   s.httpRequest.Header.Get("User-Agent"),
			Proto:       auth.ProtocolWebRTC,
			ID:          &s.uuid,
			Credentials: httpp.Credentials(s.httpRequest),
			IP:          net.ParseIP(ip),
		},
	})
	if err != nil {
		return http.StatusBadRequest, err
	}

	// site_stream_configs whitelist: checked here, before any handshake
	// work happens, so a disallowed publisher gets a definitive 403 up
	// front instead of a successful WHIP handshake followed by the
	// connection being torn down - a client like OBS doesn't reliably
	// treat a later mid-session close as "publish denied" and may just
	// keep reconnecting.
	if !s.pathManager.IsPublishAllowed(s.pathName) {
		return http.StatusForbidden, fmt.Errorf("'%s' not configured caused publish refusal!", s.pathName)
	}

	s.mutex.Lock()
	s.user = res1.User
	s.mutex.Unlock()

	iceServers, err := s.parent.generateICEServers(false)
	if err != nil {
		return http.StatusInternalServerError, err
	}

	pc := &webrtc.PeerConnection{
		Net:                   s.net,
		ICEUDPMux:             s.iceUDPMux,
		ICETCPMux:             s.iceTCPMux,
		ICEServers:            iceServers,
		IPsFromInterfaces:     s.ipsFromInterfaces,
		IPsFromInterfacesList: s.ipsFromInterfacesList,
		AdditionalHosts:       s.additionalHosts,
		STUNGatherTimeout:     time.Duration(s.stunGatherTimeout),
		Publish:               false,
		OnInboundDataChannel:  s.onInboundDataChannel,
		Log:                   s,
	}
	err = pc.Start()
	if err != nil {
		return http.StatusBadRequest, err
	}

	terminatorDone := make(chan struct{})
	defer func() { <-terminatorDone }()

	terminatorRun := make(chan struct{})
	defer close(terminatorRun)

	go func() {
		defer close(terminatorDone)
		select {
		case <-s.ctx.Done():
		case <-terminatorRun:
		}
		pc.Close()
	}()

	offer := whipOffer(s.offer)

	var sdp sdp.SessionDescription
	err = sdp.Unmarshal([]byte(offer.SDP))
	if err != nil {
		return http.StatusBadRequest, err
	}

	err = webrtc.TracksAreValid(sdp.MediaDescriptions, 1, 0)
	if err != nil {
		// RFC draft-ietf-wish-whip
		// if the number of audio and or video
		// tracks or number streams is not supported by the WHIP Endpoint, it
		// MUST reject the HTTP POST request with a "406 Not Acceptable" error
		// response.
		return http.StatusNotAcceptable, err
	}

	// Simulcast layer count is decided by the publisher (OBS), not mmx -
	// mmx unconditionally accepts any offer up to maxWHIPSimulcastLayers.
	// Above that, reject the whole POST (406, same RFC rationale as
	// TracksAreValid above) rather than silently truncating the answer's
	// RIDs: OBS checks the answer's accepted layer count against what it
	// offered (whip-output.cpp's simulcast_layers_in_answer) and already
	// treats any mismatch as a hard failure needing a retry, so failing
	// the request outright here is no worse for OBS and avoids emitting
	// an answer this server would then have to keep track of separately.
	if n := offerVideoSimulcastLayerCount(sdp.MediaDescriptions); n > maxWHIPSimulcastLayers {
		return http.StatusNotAcceptable, fmt.Errorf(
			"at most %d Simulcast layers are supported, offer requested %d", maxWHIPSimulcastLayers, n)
	}

	answer, err := pc.CreateFullAnswer(offer, false)
	if err != nil {
		return http.StatusBadRequest, err
	}

	req.res <- initialRequestRes{
		answer: []byte(answer.SDP),
	}

	go s.readRemoteCandidates(s.offer, pc)

	err = pc.WaitUntilConnected(time.Duration(s.handshakeTimeout))
	if err != nil {
		return 0, err
	}

	s.mutex.Lock()
	s.pc = pc
	s.mutex.Unlock()

	err = pc.GatherInboundTracks(time.Duration(s.trackGatherTimeout))
	if err != nil {
		return 0, err
	}

	var subStream *stream.SubStream

	medias, err := webrtc.ToStream(pc, res1.Conf, &subStream, s)
	if err != nil {
		return 0, err
	}

	res2, err := s.pathManager.AddPublisher(defs.PathAddPublisherReq{
		Author:        s,
		Desc:          &description.Session{Medias: medias},
		UseRTPPackets: true,
		ReplaceNTP:    !res1.Conf.UseAbsoluteTimestamp,
		ConfToCompare: res1.Conf,
		AccessRequest: defs.PathAccessRequest{
			Name:      s.pathName,
			Query:     s.httpRequest.URL.RawQuery,
			Publish:   true,
			SkipAuth:  true,
			UserAgent: s.httpRequest.Header.Get("User-Agent"),
		},
	})
	if err != nil {
		return 0, err
	}

	defer res2.Path.RemovePublisher(defs.PathRemovePublisherReq{Author: s})

	subStream = res2.SubStream

	pc.StartReading()

	if s.parent.degradeSampleEnabled() {
		go s.runDegradeSampling(pc)
	}

	// Publish reconnect tracking (see publishstats.go), independent of the
	// degrade protocol above: logs immediately if this session is a
	// reconnect within the current streaming period, then periodically
	// (every publishStatsSummaryInterval) reports the cumulative count and
	// how long the path has been streaming - useful to correlate operator
	// reports of "it reconnected" with real RTP loss, even when loss was
	// too brief/mild to trip the degrade FSM's 60s observation window.
	startInfo := s.parent.recordPublishSessionStart(s.pathName)
	defer s.parent.recordPublishSessionEnd(s.pathName)
	if startInfo.isReconnect {
		s.Log(logger.Warn, "[publish-stats] path=%s reconnected (likely RTP loss/network drop) - "+
			"this is reconnect #%d since streaming started at %s",
			s.pathName, startInfo.reconnectCount, startInfo.streamingSince.Format(time.RFC3339))
	}
	go s.runPublishStatsSummary()

	select {
	case <-pc.Failed():
		return 0, fmt.Errorf("peer connection closed")

	case <-s.ctx.Done():
		return 0, fmt.Errorf("terminated")
	}
}

func (s *session) runRead(req *initialRequestReq) (int, error) {
	ip, _, _ := net.SplitHostPort(s.remoteAddr)

	res, err := s.pathManager.AddReader(defs.PathAddReaderReq{
		Author: s,
		AccessRequest: defs.PathAccessRequest{
			Name:        s.pathName,
			Query:       s.httpRequest.URL.RawQuery,
			UserAgent:   s.httpRequest.Header.Get("User-Agent"),
			Proto:       auth.ProtocolWebRTC,
			ID:          &s.uuid,
			Credentials: httpp.Credentials(s.httpRequest),
			IP:          net.ParseIP(ip),
		},
	})
	if err != nil {
		if _, ok := errors.AsType[*defs.PathNoStreamAvailableError](err); ok {
			return http.StatusNotFound, err
		}

		return http.StatusBadRequest, err
	}

	defer res.Path.RemoveReader(defs.PathRemoveReaderReq{Author: s})

	s.mutex.Lock()
	s.user = res.User
	s.mutex.Unlock()

	iceServers, err := s.parent.generateICEServers(false)
	if err != nil {
		return http.StatusInternalServerError, err
	}

	pc := &webrtc.PeerConnection{
		Net:                   s.net,
		ICEUDPMux:             s.iceUDPMux,
		ICETCPMux:             s.iceTCPMux,
		ICEServers:            iceServers,
		IPsFromInterfaces:     s.ipsFromInterfaces,
		IPsFromInterfacesList: s.ipsFromInterfacesList,
		AdditionalHosts:       s.additionalHosts,
		STUNGatherTimeout:     time.Duration(s.stunGatherTimeout),
		Publish:               true,
		Log:                   s,
	}

	offer := whipOffer(s.offer)
	var offerSDP sdp.SessionDescription
	err = offerSDP.Unmarshal([]byte(offer.SDP))
	if err != nil {
		return http.StatusBadRequest, err
	}
	videoTrackCount := offerSendVideoTrackCount(offerSDP.MediaDescriptions)
	if videoTrackCount > 1 {
		videoTrackCount, err = offerH264SendTrackCount(offerSDP.MediaDescriptions)
		if err != nil {
			return http.StatusBadRequest, err
		}
		if videoTrackCount < 2 {
			return http.StatusBadRequest, fmt.Errorf("multi-layer WHEP requires H264 on every requested video track")
		}
	}

	mediaState := &webrtc.MediaState{}
	r := &stream.Reader{Parent: s, MediaFilter: mediaState.Allow}
	s.mutex.Lock()
	s.mediaState = mediaState
	s.mutex.Unlock()

	if videoTrackCount > 1 {
		err = webrtc.SetupFromStreamMultiH264(res.Stream.OrigDesc, r, pc, videoTrackCount)
		if err != nil {
			return http.StatusBadRequest, err
		}
	} else if s.abrEnabled {
		var selector *webrtc.TrackSelector
		selector = webrtc.NewTrackSelector(s, func(from, to int) {
			s.Log(logger.Info, "ABR track switched: %d → %d", from, to)
			// Send LAYER_SWITCHED via WebSocket if connected
			s.writeABRMessage(layerSwitchedMessage(to, from)) //nolint:errcheck
		})
		selector.SetOnTracksChanged(func() {
			s.writeABRMessage(tracksInfoMessage(selector)) //nolint:errcheck
		})

		if err := selector.LoadFromDescription(res.Stream.OrigDesc); err != nil {
			s.Log(logger.Warn, "ABR: failed to load tracks: %v", err)
		}

		err = webrtc.SetupFromStreamABR(res.Stream.OrigDesc, r, pc, selector)
		if err != nil {
			return http.StatusBadRequest, err
		}

		s.mutex.Lock()
		s.trackSelector = selector
		s.mutex.Unlock()

		s.Log(logger.Info, "ABR: TrackSelector loaded with %d video tracks", selector.VideoTrackCount())
	} else {
		err = webrtc.FromStream(res.Stream.OrigDesc, r, pc)
		if err != nil {
			return http.StatusBadRequest, err
		}
	}

	err = pc.Start()
	if err != nil {
		return http.StatusBadRequest, err
	}

	terminatorDone := make(chan struct{})
	defer func() { <-terminatorDone }()

	terminatorRun := make(chan struct{})
	defer close(terminatorRun)

	go func() {
		defer close(terminatorDone)
		select {
		case <-s.ctx.Done():
		case <-terminatorRun:
		}
		pc.Close()
	}()

	answer, err := pc.CreateFullAnswer(offer, false)
	if err != nil {
		return http.StatusBadRequest, err
	}

	req.res <- initialRequestRes{
		answer: []byte(answer.SDP),
	}

	go s.readRemoteCandidates(s.offer, pc)

	err = pc.WaitUntilConnected(time.Duration(s.handshakeTimeout))
	if err != nil {
		return 0, err
	}

	s.mutex.Lock()
	s.pc = pc
	s.mutex.Unlock()

	s.Log(logger.Info, "is reading from path '%s', %s",
		res.Path.Name(), defs.FormatsInfo(r.Formats()))

	onUnreadHook := hooks.OnRead(hooks.OnReadParams{
		Logger:          s,
		ExternalCmdPool: s.externalCmdPool,
		Conf:            res.Path.SafeConf(),
		ExternalCmdEnv:  res.Path.ExternalCmdEnv(),
		Reader:          *s.APIReaderDescribe(),
		Query:           s.httpRequest.URL.RawQuery,
	})
	defer onUnreadHook()

	res.Stream.AddReader(r)
	defer res.Stream.RemoveReader(r)

	s.mutex.Lock()
	s.reader = r
	s.abrReady = true
	s.mutex.Unlock()

	// Subscribe to this path's obs-timestamp fan-out (see
	// obs_timestamp_broadcast.go) so p2p delay can be computed client-side.
	s.parent.subscribeObsTimestamp(s.pathName, s)
	defer s.parent.unsubscribeObsTimestamp(s.pathName, s)

	select {
	case <-pc.Failed():
		return 0, fmt.Errorf("peer connection closed")

	case err = <-r.Error():
		return 0, err

	case <-s.ctx.Done():
		return 0, fmt.Errorf("terminated")
	}
}

func (s *session) readRemoteCandidates(offer []byte, pc *webrtc.PeerConnection) {
	remoteUfrag := parseOfferUfrag(offer)

	for {
		select {
		case req := <-s.chAddCandidates:
			// do not check for errors since credentials are optional
			ufrag, pwd, _ := sdpFragmentToCredentials(req.fragment)

			candidates, err := sdpFragmentToCandidates(req.fragment)
			if err != nil {
				req.res <- addSessionCandidatesRes{err: err}
				continue
			}

			// ICE restart: client sent new credentials
			var answer *pwebrtc.SessionDescription
			if ufrag != "" && ufrag != remoteUfrag {
				sdp := replaceICECredentials(offer, ufrag, pwd)

				answer, err = pc.CreateFullAnswer(whipOffer(sdp), true)
				if err != nil {
					req.res <- addSessionCandidatesRes{err: err}
					continue
				}
			}

			var addErr error
			for _, candidate := range candidates {
				addErr = pc.AddRemoteCandidate(candidate)
				if addErr != nil {
					break
				}
			}
			if addErr != nil {
				req.res <- addSessionCandidatesRes{err: addErr}
				continue
			}

			if ufrag != "" && ufrag != remoteUfrag {
				var frag *whip.SDPFragment
				frag, err = fullAnswerToSDPFragment(answer.SDP)
				if err != nil {
					req.res <- addSessionCandidatesRes{err: err}
					continue
				}

				remoteUfrag = ufrag
				req.res <- addSessionCandidatesRes{answer: frag}
			} else {
				req.res <- addSessionCandidatesRes{}
			}

		case <-s.ctx.Done():
			return
		}
	}
}

// onInboundDataChannel is registered as PeerConnection.OnInboundDataChannel
// for WHIP publish sessions. Only reacts to the "obs-timestamp" channel
// (see docs/obs-abs-timestamp-protocol.md in the OBS repo); any other
// channel a publisher happens to create is left alone.
func (s *session) onInboundDataChannel(dc *pwebrtc.DataChannel) {
	if dc.Label() != webrtc.OBSTimestampDataChannelLabel {
		return
	}

	dc.OnMessage(func(msg pwebrtc.DataChannelMessage) {
		ts, err := webrtc.ParseOBSTimestampMessage(msg.Data)
		if err != nil {
			s.Log(logger.Debug, "obs-timestamp: invalid message: %v", err)
			return
		}

		latencyMs := float64(time.Now().UnixMilli() - int64(ts.TimestampMS))

		s.mutex.Lock()
		s.obsTimestampLatencyMs = &latencyMs
		s.mutex.Unlock()

		// Relay to this path's WHEP readers (see
		// obs_timestamp_broadcast.go) so a player can compute true
		// end-to-end p2p delay, not just the OBS-to-mmx leg above.
		s.parent.broadcastObsTimestamp(s.pathName, obsTimestampMessage(ts))
	})
}

// degradeStatsLogInterval is how often runDegradeSampling logs the
// received bitrate / active simulcast layer count - independent of
// degradeSampleInterval (the FSM's 1s loss-sampling cadence, which stays
// fine-grained since it drives the 60s observation window).
const degradeStatsLogInterval = 5 * time.Second

// runDegradeSampling periodically feeds this publish session's cumulative
// RTP loss/received counters into the path's degrade FSM (see
// docs/obs-mmx-degrade-protocol.md), and separately logs a live
// bitrate/layer-count summary every degradeStatsLogInterval - useful for
// telling apart "OBS's own WebRTC congestion control already throttled
// the send bitrate (or paused a layer) down on its own" from "the
// configured bitrate/layers are actually reaching us and still causing
// loss". Only meaningful once pc.StartReading has been called
// (PeerConnection.Stats only reports inbound-track stats after that
// point).
func (s *session) runDegradeSampling(pc *webrtc.PeerConnection) {
	videoLayers := 0
	for _, tr := range pc.InboundTracks() {
		if tr.Kind() == pwebrtc.RTPCodecTypeVideo {
			videoLayers++
		}
	}
	s.parent.observeDegradeSessionLayers(s.pathName, videoLayers)

	sampleTicker := time.NewTicker(degradeSampleInterval)
	defer sampleTicker.Stop()
	statsTicker := time.NewTicker(degradeStatsLogInterval)
	defer statsTicker.Stop()

	var lastBytesReceived uint64
	haveLastBytes := false
	lastTrackReceived := map[*webrtc.InboundTrack]uint64{}

	for {
		select {
		case <-sampleTicker.C:
			stats := pc.Stats()
			s.parent.recordDegradeSample(s.pathName, stats.RTPPacketsLost, stats.RTPPacketsReceived)

		case <-statsTicker.C:
			stats := pc.Stats()
			var kbps float64
			if haveLastBytes && stats.BytesReceived >= lastBytesReceived {
				kbps = float64(stats.BytesReceived-lastBytesReceived) * 8 / 1000 / degradeStatsLogInterval.Seconds()
			}
			lastBytesReceived = stats.BytesReceived
			haveLastBytes = true

			activeLayers := 0
			for _, tr := range pc.InboundTracks() {
				if tr.Kind() != pwebrtc.RTPCodecTypeVideo {
					continue
				}
				received := tr.Stats().TotalReceived
				if received > lastTrackReceived[tr] {
					activeLayers++
				}
				lastTrackReceived[tr] = received
			}

			s.Log(logger.Info, "[degrade] received bitrate=%.0fkbps active simulcast layers=%d", kbps, activeLayers)

		case <-s.ctx.Done():
			return
		}
	}
}

// publishStatsSummaryInterval is how often an active WHIP publish session
// logs the cumulative reconnect count / streaming duration for its path
// (see publishstats.go). Independent of any degrade-protocol logging.
const publishStatsSummaryInterval = 180 * time.Second

// runPublishStatsSummary periodically logs how long this path has been
// continuously streaming and how many WHIP publish reconnects have
// happened during that period - only while a publish session is actually
// active, so a path with no publisher doesn't accumulate log noise.
func (s *session) runPublishStatsSummary() {
	ticker := time.NewTicker(publishStatsSummaryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			elapsed, reconnects := s.parent.publishStatsSummary(s.pathName)
			s.Log(logger.Info, "[publish-stats] path=%s streaming for %s, %d reconnect(s) so far",
				s.pathName, elapsed.Round(time.Second), reconnects)

		case <-s.ctx.Done():
			return
		}
	}
}

func (s *session) initialRequest(req initialRequestReq) initialRequestRes {
	req.res = make(chan initialRequestRes)

	select {
	case s.chInitialRequest <- req:
		return <-req.res

	case <-s.ctx.Done():
		return initialRequestRes{err: fmt.Errorf("terminated"), errStatusCode: http.StatusInternalServerError}
	}
}

// addCandidates is called by webRTCHTTPServer through Server.
func (s *session) addCandidates(
	req addSessionCandidatesReq,
) addSessionCandidatesRes {
	select {
	case s.chAddCandidates <- req:
		return <-req.res

	case <-s.ctx.Done():
		return addSessionCandidatesRes{err: fmt.Errorf("terminated")}
	}
}

// APIReaderDescribe implements reader.
func (s *session) APIReaderDescribe() *defs.APIPathReader {
	return &defs.APIPathReader{
		Type: defs.APIPathReaderTypeWebRTCSession,
		ID:   s.uuid.String(),
	}
}

// APISourceDescribe implements source.
func (s *session) APISourceDescribe() *defs.APIPathSource {
	return &defs.APIPathSource{
		Type: defs.APIPathSourceTypeWebRTCSession,
		ID:   s.uuid.String(),
	}
}

func (s *session) apiItem() *defs.APIWebRTCSession {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	peerConnectionEstablished := false
	localCandidate := ""
	remoteCandidate := ""
	bytesReceived := uint64(0)
	bytesSent := uint64(0)
	rtpPacketsReceived := uint64(0)
	rtpPacketsSent := uint64(0)
	rtpPacketsLost := uint64(0)
	rtpPacketsJitter := float64(0)
	rtcpPacketsReceived := uint64(0)
	rtcpPacketsSent := uint64(0)
	outboundFramesDiscarded := uint64(0)

	if s.pc != nil {
		peerConnectionEstablished = true
		localCandidate = s.pc.LocalCandidate()
		remoteCandidate = s.pc.RemoteCandidate()
		stats := s.pc.Stats()
		bytesReceived = stats.BytesReceived
		bytesSent = stats.BytesSent
		rtpPacketsReceived = stats.RTPPacketsReceived
		rtpPacketsSent = stats.RTPPacketsSent
		rtpPacketsLost = stats.RTPPacketsLost
		rtpPacketsJitter = stats.RTPPacketsJitter
		rtcpPacketsReceived = stats.RTCPPacketsReceived
		rtcpPacketsSent = stats.RTCPPacketsSent
	}

	if s.reader != nil {
		outboundFramesDiscarded = s.reader.OutboundFramesDiscarded()
	}

	return &defs.APIWebRTCSession{
		ID:                        s.uuid,
		Created:                   s.created,
		RemoteAddr:                s.remoteAddr,
		PeerConnectionEstablished: peerConnectionEstablished,
		LocalCandidate:            localCandidate,
		RemoteCandidate:           remoteCandidate,
		State: func() defs.APIWebRTCSessionState {
			if s.publish {
				return defs.APIWebRTCSessionStatePublish
			}
			return defs.APIWebRTCSessionStateRead
		}(),
		Path:                    s.pathName,
		Query:                   s.httpRequest.URL.RawQuery,
		User:                    s.user,
		UserAgent:               s.httpRequest.Header.Get("User-Agent"),
		InboundBytes:            bytesReceived,
		InboundRTPPackets:       rtpPacketsReceived,
		InboundRTPPacketsLost:   rtpPacketsLost,
		InboundRTPPacketsJitter: rtpPacketsJitter,
		InboundRTCPPackets:      rtcpPacketsReceived,
		OutboundBytes:           bytesSent,
		OutboundRTPPackets:      rtpPacketsSent,
		OutboundRTCPPackets:     rtcpPacketsSent,
		OutboundFramesDiscarded: outboundFramesDiscarded,
		OBSTimestampLatencyMs:   s.obsTimestampLatencyMs,
		BytesReceived:           bytesReceived,
		BytesSent:               bytesSent,
		RTPPacketsReceived:      rtpPacketsReceived,
		RTPPacketsSent:          rtpPacketsSent,
		RTPPacketsLost:          rtpPacketsLost,
		RTPPacketsJitter:        rtpPacketsJitter,
		RTCPPacketsReceived:     rtcpPacketsReceived,
		RTCPPacketsSent:         rtcpPacketsSent,
	}
}

// RequestKeyFrame sends a PLI RTCP packet to the publisher
// to request an immediate keyframe, reducing viewer startup latency.
func (s *session) RequestKeyFrame() error {
	s.mutex.RLock()
	pc := s.pc
	isPub := s.publish
	s.mutex.RUnlock()

	if !isPub || pc == nil {
		return nil
	}

	return pc.SendPLI()
}
