// Package srt contains a SRT server.
package srt

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"

	srt "github.com/datarhei/gosrt"
	"github.com/google/uuid"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/degrade"
	"github.com/bluenviron/mediamtx/internal/externalcmd"
	"github.com/bluenviron/mediamtx/internal/logger"
)

// ErrConnNotFound is returned when a connection is not found.
var ErrConnNotFound = errors.New("connection not found")

func interfaceIsEmpty(i any) bool {
	return reflect.ValueOf(i).Kind() != reflect.Pointer || reflect.ValueOf(i).IsNil()
}

func srtMaxPayloadSize(u int) int {
	return ((u - 16) / 188) * 188 // 16 = SRT header, 188 = MPEG-TS packet
}

type serverAPIConnsListRes struct {
	data *defs.APISRTConnList
	err  error
}

type serverAPIConnsListReq struct {
	res chan serverAPIConnsListRes
}

type serverAPIConnsGetRes struct {
	data *defs.APISRTConn
	err  error
}

type serverAPIConnsGetReq struct {
	uuid uuid.UUID
	res  chan serverAPIConnsGetRes
}

type serverAPIConnsKickRes struct {
	err error
}

type serverAPIConnsKickReq struct {
	uuid uuid.UUID
	res  chan serverAPIConnsKickRes
}

type serverMetrics interface {
	SetSRTServer(defs.APISRTServer)
}

type serverPathManager interface {
	FindPathConf(req defs.PathFindPathConfReq) (*defs.PathFindPathConfRes, error)
	AddPublisher(req defs.PathAddPublisherReq) (*defs.PathAddPublisherRes, error)
	AddReader(req defs.PathAddReaderReq) (*defs.PathAddReaderRes, error)
}

type serverParent interface {
	logger.Writer
}

// Server is a SRT server.
type Server struct {
	Address             string
	RTSPAddress         string
	ReadTimeout         conf.Duration
	WriteTimeout        conf.Duration
	UDPMaxPayloadSize   int
	Latency             conf.Duration
	FC                  uint
	RunOnConnect        string
	RunOnConnectRestart bool
	RunOnDisconnect     string
	ExternalCmdPool     *externalcmd.Pool
	Metrics             serverMetrics
	PathManager         serverPathManager
	// PublishAuthKey is the shared secret ppcenter seals publish tokens
	// with - the same key the WHIP server uses (see the HEVC/H264
	// multitrack design §3.2: both ingest protocols share one token
	// format, one key and one codec-binding rule). Empty disables token
	// checking entirely, leaving the pre-existing streamID user/pass path.
	PublishAuthKey string
	// PublishTokenRequired rejects a publish that carries no token at all.
	// Off by default so existing SRT publishers and third-party tools keep
	// working; production should turn it on.
	PublishTokenRequired bool
	// LossAlarmReporter sends a report to ppcenter whenever a publish
	// connection's SRT loss rate crosses LossAlarmThresholdPct (and once
	// more when it drops back under it). Nil disables reporting outright
	// regardless of LossAlarmEnable, so a deployment with mmxControl off
	// never needs to touch these fields at all.
	LossAlarmReporter     srtLossAlarmReporter
	LossAlarmEnable       bool
	LossAlarmThresholdPct float64
	// LossDisconnectEnable forces a publish connection closed once its loss
	// rate has stayed above LossAlarmThresholdPct continuously for
	// LossDisconnectSec, so a wedged OBS publisher is made to reconnect
	// (and typically renegotiate a bitrate the link can carry) instead of
	// silently degrading indefinitely.
	LossDisconnectEnable bool
	LossDisconnectSec    int
	// DegradeManager is the shared per-path degrade FSM registry (see
	// internal/degrade), constructed and owned by internal/core alongside
	// the webrtc.Server that actually serves the degrade WS channel - nil
	// when WebRTC is disabled, in which case DegradeEnable is forced false
	// by the caller (there would be nowhere for the executor to connect).
	DegradeManager        *degrade.Manager
	DegradeEnable         bool
	DegradeInstantLossPct float64
	DegradeAvgLossPct     float64
	// RecoverInstantLossPct/RecoverAvgLossPct are the hysteresis "recover"
	// thresholds paired with DegradeInstantLossPct/DegradeAvgLossPct above -
	// see the Thresholds doc comment in internal/degrade for why they're
	// separate.
	RecoverInstantLossPct float64
	RecoverAvgLossPct     float64
	DegradeObservationSec int
	Parent                serverParent

	ctx       context.Context
	ctxCancel func()
	wg        sync.WaitGroup
	ln        srt.Listener
	conns     map[*conn]struct{}

	// in
	chNewConnRequest chan srt.ConnRequest
	chAcceptErr      chan error
	chCloseConn      chan *conn
	chAPIConnsList   chan serverAPIConnsListReq
	chAPIConnsGet    chan serverAPIConnsGetReq
	chAPIConnsKick   chan serverAPIConnsKickReq
}

// Initialize initializes the server.
func (s *Server) Initialize() error {
	conf := srt.DefaultConfig()
	conf.ConnectionTimeout = time.Duration(s.ReadTimeout)
	conf.PeerIdleTimeout = time.Duration(s.ReadTimeout)
	conf.PayloadSize = uint32(srtMaxPayloadSize(s.UDPMaxPayloadSize))

	// Latency sets gosrt's Config.Latency, which in turn drives both
	// PeerLatency and ReceiverLatency (see gosrt's Config.Validate()) -
	// the TSBPD delivery delay that gives a NAK-triggered retransmit time
	// to arrive before its packet is considered lost.
	//
	// This is the single most important setting for SRT ingest loss.
	// gosrt defaults it to 120ms, which on a typical ~32ms-RTT publish
	// link is only ~3.75x RTT - the bare minimum for one retransmit round
	// trip, with no margin for jitter. When RTT briefly rises, retransmits
	// miss the TSBPD deadline and get counted as loss despite having
	// arrived, which shows up as a loss-rate spike with a flat RTT graph.
	// conf.SRTLatency defaults to 2000ms for that reason; see its field
	// doc in internal/conf for the sizing rule.
	if s.Latency > 0 {
		conf.Latency = time.Duration(s.Latency)
	}
	// FC must scale with Latency: it bounds how many packets may be in
	// flight unacknowledged, so a large latency window is unusable if
	// flow control won't permit that many outstanding packets. gosrt also
	// advertises it to the peer as MaxFlowWindowSize and reports it as
	// AvailableBufferSize.
	if s.FC > 0 {
		conf.FC = uint32(s.FC)
	}

	// Deliberately not set here: gosrt v0.11.0's Config.ReceiverBufferSize
	// (SRTO_RCVBUF) is a dead field - it appears only in the struct, its
	// zero default, and srt:// query-string parse/marshal. Nothing in the
	// library ever applies it to the socket or to any internal buffer
	// (ListenControl() in net.go only sets SO_REUSEADDR/IP_TOS/IP_TTL,
	// and no code path calls setsockopt(SO_RCVBUF)). Setting it would be
	// a silent no-op, so FC above carries the receive-window sizing
	// instead. Likewise there is no OS-level UDP read buffer tuning for
	// SRT the way there is for WebRTC/RTSP-UDP/MoQ
	// (internal/protocols/udpreadbuffer), short of forking gosrt.
	//
	// The OS-side counterpart to these settings is
	// net.core.netdev_max_backlog, which defaults to 1000 and should be
	// >= 5000 for multi-layer simulcast ingest - see
	// scripts/tune-udp-buffers.sh.

	var err error
	s.ln, err = srt.Listen("srt", s.Address, conf)
	if err != nil {
		return err
	}

	s.ctx, s.ctxCancel = context.WithCancel(context.Background())

	s.conns = make(map[*conn]struct{})
	s.chNewConnRequest = make(chan srt.ConnRequest)
	s.chAcceptErr = make(chan error)
	s.chCloseConn = make(chan *conn)
	s.chAPIConnsList = make(chan serverAPIConnsListReq)
	s.chAPIConnsGet = make(chan serverAPIConnsGetReq)
	s.chAPIConnsKick = make(chan serverAPIConnsKickReq)

	s.Log(logger.Info, "started with listener on "+s.Address+" (UDP/SRT)")

	l := &listener{
		ln:     s.ln,
		wg:     &s.wg,
		parent: s,
	}
	l.initialize()

	s.wg.Add(1)
	go s.run()

	if !interfaceIsEmpty(s.Metrics) {
		s.Metrics.SetSRTServer(s)
	}

	return nil
}

// Log implements logger.Writer.
func (s *Server) Log(level logger.Level, format string, args ...any) {
	s.Parent.Log(level, "[SRT] "+format, args...)
}

// Close closes the server.
func (s *Server) Close() {
	s.Log(logger.Info, "closing")

	if !interfaceIsEmpty(s.Metrics) {
		s.Metrics.SetSRTServer(nil)
	}

	s.ctxCancel()
	s.wg.Wait()
}

func (s *Server) run() {
	defer s.wg.Done()

outer:
	for {
		select {
		case err := <-s.chAcceptErr:
			s.Log(logger.Error, "%s", err)
			break outer

		case req := <-s.chNewConnRequest:
			c := &conn{
				parentCtx:           s.ctx,
				rtspAddress:         s.RTSPAddress,
				readTimeout:         s.ReadTimeout,
				writeTimeout:        s.WriteTimeout,
				udpMaxPayloadSize:   s.UDPMaxPayloadSize,
				connReq:             req,
				runOnConnect:        s.RunOnConnect,
				runOnConnectRestart: s.RunOnConnectRestart,
				runOnDisconnect:     s.RunOnDisconnect,
				wg:                  &s.wg,
				externalCmdPool:     s.ExternalCmdPool,
				pathManager:         s.PathManager,
				publishAuthKey:      s.PublishAuthKey,
				publishTokenReq:     s.PublishTokenRequired,
				parent:              s,

				lossAlarmReporter:     s.LossAlarmReporter,
				lossAlarmEnable:       s.LossAlarmEnable,
				lossAlarmThresholdPct: s.LossAlarmThresholdPct,
				lossDisconnectEnable:  s.LossDisconnectEnable,
				lossDisconnectSec:     s.LossDisconnectSec,

				degradeManager:        s.DegradeManager,
				degradeEnable:         s.DegradeEnable,
				degradeInstantLossPct: s.DegradeInstantLossPct,
				degradeAvgLossPct:     s.DegradeAvgLossPct,
				recoverInstantLossPct: s.RecoverInstantLossPct,
				recoverAvgLossPct:     s.RecoverAvgLossPct,
				degradeObservationSec: s.DegradeObservationSec,
			}
			c.initialize()
			s.conns[c] = struct{}{}

		case c := <-s.chCloseConn:
			delete(s.conns, c)

		case req := <-s.chAPIConnsList:
			data := &defs.APISRTConnList{
				Items: []defs.APISRTConn{},
			}

			for c := range s.conns {
				data.Items = append(data.Items, *c.apiItem())
			}

			sort.Slice(data.Items, func(i, j int) bool {
				return data.Items[i].Created.Before(data.Items[j].Created)
			})

			req.res <- serverAPIConnsListRes{data: data}

		case req := <-s.chAPIConnsGet:
			c := s.findConnByUUID(req.uuid)
			if c == nil {
				req.res <- serverAPIConnsGetRes{err: ErrConnNotFound}
				continue
			}

			req.res <- serverAPIConnsGetRes{data: c.apiItem()}

		case req := <-s.chAPIConnsKick:
			c := s.findConnByUUID(req.uuid)
			if c == nil {
				req.res <- serverAPIConnsKickRes{err: ErrConnNotFound}
				continue
			}

			delete(s.conns, c)
			c.Close()
			req.res <- serverAPIConnsKickRes{}

		case <-s.ctx.Done():
			break outer
		}
	}

	s.ctxCancel()

	s.ln.Close()
}

func (s *Server) findConnByUUID(uuid uuid.UUID) *conn {
	for sx := range s.conns {
		if sx.uuid == uuid {
			return sx
		}
	}
	return nil
}

// newConnRequest is called by srtListener.
func (s *Server) newConnRequest(connReq srt.ConnRequest) {
	select {
	case s.chNewConnRequest <- connReq:
	case <-s.ctx.Done():
		connReq.Reject(srt.REJ_CLOSE)
	}
}

// acceptError is called by srtListener.
func (s *Server) acceptError(err error) {
	select {
	case s.chAcceptErr <- err:
	case <-s.ctx.Done():
	}
}

// closeConn is called by conn.
func (s *Server) closeConn(c *conn) {
	select {
	case s.chCloseConn <- c:
	case <-s.ctx.Done():
	}
}

// APIConnsList implements defs.APISRTServer.
func (s *Server) APIConnsList() (*defs.APISRTConnList, error) {
	req := serverAPIConnsListReq{
		res: make(chan serverAPIConnsListRes),
	}

	select {
	case s.chAPIConnsList <- req:
		res := <-req.res
		return res.data, res.err

	case <-s.ctx.Done():
		return nil, fmt.Errorf("terminated")
	}
}

// APIConnsGet implements defs.APISRTServer.
func (s *Server) APIConnsGet(uuid uuid.UUID) (*defs.APISRTConn, error) {
	req := serverAPIConnsGetReq{
		uuid: uuid,
		res:  make(chan serverAPIConnsGetRes),
	}

	select {
	case s.chAPIConnsGet <- req:
		res := <-req.res
		return res.data, res.err

	case <-s.ctx.Done():
		return nil, fmt.Errorf("terminated")
	}
}

// APIConnsKick implements defs.APISRTServer.
func (s *Server) APIConnsKick(uuid uuid.UUID) error {
	req := serverAPIConnsKickReq{
		uuid: uuid,
		res:  make(chan serverAPIConnsKickRes),
	}

	select {
	case s.chAPIConnsKick <- req:
		res := <-req.res
		return res.err

	case <-s.ctx.Done():
		return fmt.Errorf("terminated")
	}
}
