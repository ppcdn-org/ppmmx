// Package srt contains a SRT server.
package srt

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"sync"
	"time"

	srt "github.com/datarhei/gosrt"
	"github.com/google/uuid"

	"code.cloudfoundry.org/bytefmt"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/degrade"
	"github.com/bluenviron/mediamtx/internal/externalcmd"
	"github.com/bluenviron/mediamtx/internal/logger"
)

// ErrConnNotFound is returned when a connection is not found.
var ErrConnNotFound = errors.New("connection not found")

// srtLatencyAssumedBitrateBps and srtLatencyBufferSafetyFactor size the
// receive buffer/FC for the worst case a path can ever be tuned to
// (SRTLatencyMax), rather than per-connection for the currently tuned
// value - see docs/srt-adaptive-latency-design.md's "Buffer sizing"
// section for why: a later raise could otherwise outgrow a buffer already
// allocated for a lower value, which is exactly the problem this whole
// mechanism exists to avoid for latency itself. The bitrate is a
// conservative estimate for a 3-layer simulcast ingest, doubled for
// headroom.
const (
	srtLatencyAssumedBitrateBps  = 12e6
	srtLatencyBufferSafetyFactor = 2

	// srtWorstCasePayloadSize mirrors conf.srtMinPayloadSize (7 MPEG-TS
	// packets of 188 bytes, the smallest payload a sender is likely to
	// use) - duplicated here rather than imported since that constant is
	// unexported and this package must stay decoupled from conf's
	// internals for the same worst-case-packet-count reasoning.
	srtWorstCasePayloadSize = 7 * 188
)

func interfaceIsEmpty(i any) bool {
	return reflect.ValueOf(i).Kind() != reflect.Pointer || reflect.ValueOf(i).IsNil()
}

func srtMaxPayloadSize(u int) int {
	return ((u - 16) / 188) * 188 // 16 = SRT header, 188 = MPEG-TS packet
}

// srtWorstCaseBuffer returns the receive buffer size (bytes) and flow
// control window (packets) needed to hold latencyMax worth of stream at
// srtLatencyAssumedBitrateBps, never going below the configured minimums.
func srtWorstCaseBuffer(latencyMax time.Duration, minBufBytes uint64, minFC int) (uint64, int) {
	bufBytes := uint64(latencyMax.Seconds() * srtLatencyAssumedBitrateBps / 8 * srtLatencyBufferSafetyFactor)
	if bufBytes < minBufBytes {
		bufBytes = minBufBytes
	}

	fc := int(math.Ceil(float64(bufBytes) / srtWorstCasePayloadSize))
	if fc < minFC {
		fc = minFC
	}

	return bufBytes, fc
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
	RunOnConnect        string
	RunOnConnectRestart bool
	RunOnDisconnect     string
	ExternalCmdPool     *externalcmd.Pool
	Metrics             serverMetrics
	PathManager         serverPathManager

	// SRT transport tuning, applied to every accepted connection (see
	// conf.SRTLatency's doc comment for why gosrt's own 120ms/unset
	// defaults are unusable for a WAN ingest). Latency is applied to
	// both the receiver and peer latency socket options.
	Latency            conf.Duration
	ReceiverBufferSize conf.StringSize
	FlowControlWindow  int

	// SRT adaptive receive latency (see conf.SRTLatencyAutoTune's doc
	// comment and docs/srt-adaptive-latency-design.md). When disabled,
	// every connection uses Latency/ReceiverBufferSize/FlowControlWindow
	// above unchanged - byte for byte the pre-existing behavior.
	LatencyAutoTune  bool
	LatencyMin       conf.Duration
	LatencyMax       conf.Duration
	LatencyStep      conf.Duration
	LatencyRaiseStep conf.Duration
	LatencyRaisePct  float64
	LatencyLowerPct  float64
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
	// LossSampleReporter persists a per-minute recoverable/unrecoverable loss
	// breakdown to ppcenter, independent of any threshold - the trend view,
	// as opposed to LossAlarmReporter's event-driven alarm. Nil disables it,
	// same as the alarm reporter (see core.go's wiring gating).
	LossSampleReporter srtLossSampleReporter
	// LossDisconnectEnable forces a publish connection closed once its loss
	// rate has stayed above LossAlarmThresholdPct continuously for
	// LossDisconnectSec, so a wedged OBS publisher is made to reconnect
	// (and typically renegotiate a bitrate the link can carry) instead of
	// silently degrading indefinitely.
	LossDisconnectEnable bool
	LossDisconnectSec    int
	// LossRecycleEnable is a second, slower disconnect tier below
	// LossDisconnect (see conf.SRTLossRecycleEnable): chronic MILD
	// unrecoverable loss (LossRecycleThresholdPct, default lower than the
	// alarm) sustained for LossRecycleSec (default 1h, far longer than
	// LossDisconnectSec) forces a reconnect. Its own tracker, so the two
	// thresholds don't interfere. Also independent of MMXControl.
	LossRecycleEnable       bool
	LossRecycleThresholdPct float64
	LossRecycleSec          int
	// PublishSessionReporter reports a publish connection's start/end to
	// ppcenter (see publish_session_report.go). Nil when MMXControl isn't
	// set up - same construction gating as the reporters above.
	PublishSessionReporter publishSessionReporter
	// DegradeManager is the shared per-path degrade FSM registry (see
	// internal/degrade), constructed and owned by internal/core alongside
	// the webrtc.Server that actually serves the degrade WS channel - nil
	// when WebRTC is disabled, in which case DegradeEnable is forced false
	// by the caller (there would be nowhere for the executor to connect).
	DegradeManager *degrade.Manager
	DegradeEnable  bool
	// DegradeRaisePct/DegradeLowerPct are the unified ULR thresholds, and
	// DegradeRestartPct gates the disruptive layer phase / forced reconnect
	// (see docs/design/publish-degrade-protocol.zh-CN.md). Applied to the
	// connection's UNRECOVERABLE loss rate via runDegradeSampling.
	DegradeRaisePct        float64
	DegradeLowerPct        float64
	DegradeRestartPct      float64
	DegradeObservationSec  int
	DegradeSampleSec       int
	DegradeRaiseLatencyStep time.Duration
	DegradeLowerLatencyStep time.Duration
	DegradeLatencyMin      time.Duration
	DegradeLatencyMax      time.Duration
	Parent                serverParent

	ctx            context.Context
	ctxCancel      func()
	wg             sync.WaitGroup
	ln             srt.Listener
	conns          map[*conn]struct{}
	srtLogger      srt.Logger
	latencyManager *latencyManager

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
	bufSize := uint64(s.ReceiverBufferSize)
	fc := s.FlowControlWindow
	// Latency is tuned either by the legacy per-minute auto-tuner
	// (LatencyAutoTune) or by the unified degrade protocol (DegradeEnable),
	// so the worst-case buffer must be allocated whenever either is on.
	latencyTuning := s.LatencyAutoTune || s.DegradeEnable
	// Effective latency-tuning parameters: the unified degrade protocol
	// owns them when enabled (its own step/bounds and a single raise
	// threshold shared with the degrade trigger), otherwise the legacy
	// auto-tuner's config applies. Only the startup log and the
	// (degrade-gated) legacy Record path read these.
	effLatencyMin := time.Duration(s.LatencyMin)
	effLatencyMax := time.Duration(s.LatencyMax)
	effLatencyStep := time.Duration(s.LatencyStep)
	effLatencyRaiseStep := time.Duration(s.LatencyRaiseStep)
	effRaisePct := s.LatencyRaisePct
	effLowerPct := s.LatencyLowerPct
	tuningSource := "legacy auto-tune"
	if s.DegradeEnable {
		tuningSource = "unified degrade"
		effLatencyMin = time.Duration(s.DegradeLatencyMin)
		effLatencyMax = time.Duration(s.DegradeLatencyMax)
		effLatencyStep = time.Duration(s.DegradeLowerLatencyStep)
		effLatencyRaiseStep = time.Duration(s.DegradeRaiseLatencyStep)
		effRaisePct = s.DegradeRaisePct
		effLowerPct = s.DegradeLowerPct
	}
	if latencyTuning {
		// Sized for the largest latency ceiling any path can reach (the
		// legacy auto-tuner's LatencyMax and/or the unified
		// DegradeLatencyMax), not for the current value - see
		// srtWorstCaseBuffer's doc comment.
		latencyMax := effLatencyMax
		if d := time.Duration(s.LatencyMax); d > latencyMax {
			latencyMax = d
		}
		bufSize, fc = srtWorstCaseBuffer(latencyMax, bufSize, fc)
	}

	conf := srt.DefaultConfig()
	conf.ConnectionTimeout = time.Duration(s.ReadTimeout)
	conf.PeerIdleTimeout = time.Duration(s.ReadTimeout)
	conf.PayloadSize = uint32(srtMaxPayloadSize(s.UDPMaxPayloadSize))

	// Latency sets both PeerLatency and ReceiverLatency - the TSBPD
	// delivery delay that gives a NAK-triggered retransmit time to arrive
	// before its packet is considered lost.
	//
	// This is the single most important setting for SRT ingest loss.
	// gosrt defaults it to 120ms, which on a typical ~32ms-RTT publish
	// link is only ~3.75x RTT - the bare minimum for one retransmit round
	// trip, with no margin for jitter. When RTT briefly rises, retransmits
	// miss the TSBPD deadline and get counted as loss despite having
	// arrived, which shows up as a loss-rate spike with a flat RTT graph.
	// This is the listener-wide starting point; if LatencyAutoTune is on,
	// newConnRequest overrides it per path per connection - see
	// docs/srt-adaptive-latency-design.md.
	if s.Latency > 0 {
		conf.ReceiverLatency = time.Duration(s.Latency)
		conf.PeerLatency = time.Duration(s.Latency)
	}
	conf.ReceiverBufferSize = uint32(bufSize)
	if fc > 0 {
		conf.FC = uint32(fc)
	}

	// Debug-only visibility into gosrt's own NAK control-packet trace, for
	// diagnosing cases where gosrt's receive-side loss/retrans/drop
	// accounting doesn't reconcile against the publisher's own SRT stack
	// (e.g. a libsrt sender reporting ~0% retransmitted while gosrt records
	// real retrans/drop activity - seen on a real publish session, most
	// consistent with reordering/duplication on the wire being attributed
	// to loss+retransmit by gosrt without libsrt ever actually
	// retransmitting for it). Always subscribed; genuinely silent unless
	// logLevel is set to debug, same as every other logger.Debug call in
	// this package - see internal/logger.Logger.Log's level filter.
	conf.Logger = srt.NewLogger([]string{"control:send:NAK", "control:recv:NAK"})
	s.srtLogger = conf.Logger

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
	s.Log(logger.Info, "SRT receive window: latency %v, buffer %s, flow control window %d packets",
		time.Duration(s.Latency), bytefmt.ByteSize(bufSize), fc)

	if latencyTuning {
		s.latencyManager = newLatencyManager(srtLatencyConfig{
			Initial:   time.Duration(s.Latency),
			Min:       effLatencyMin,
			Max:       effLatencyMax,
			Step:      effLatencyStep,
			RaiseStep: effLatencyRaiseStep,
			RaisePct:  effRaisePct,
			LowerPct:  effLowerPct,
		}, s.Log)

		s.Log(logger.Info, "SRT latency tuning: enabled (%s), range [%v, %v], raise step %v, "+
			"lower step %v, raise/lower thresholds %.2f%%/%.2f%%",
			tuningSource, effLatencyMin, effLatencyMax,
			effLatencyRaiseStep, effLatencyStep, effRaisePct, effLowerPct)
	}

	// Forwards gosrt's NAK trace into our own logger. Exits via s.ctx rather
	// than draining s.srtLogger.Listen() to closure - closing a gosrt
	// Logger while a connection goroutine might still be calling Print() on
	// it panics (send on closed channel), and s.ctx is already the
	// mechanism that guarantees every conn goroutine has stopped by the
	// time Close() returns (see s.wg.Wait() there), so there's nothing to
	// gain from also calling srtLogger.Close().
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			select {
			case <-s.ctx.Done():
				return
			case m, ok := <-s.srtLogger.Listen():
				if !ok {
					return
				}
				s.Log(logger.Debug, "nak socket=%#08x topic=%s: %s", m.SocketId, m.Topic, m.Message)
			}
		}
	}()

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
			if s.latencyManager != nil {
				// StreamId is available before Accept, so the path can be
				// resolved and this connection's latency overridden ahead
				// of the handshake completing - see streamID.unmarshal
				// (conn.go) for the format and docs/srt-adaptive-latency-
				// design.md for why this must be per-request, not
				// per-listener.
				var sid streamID
				if err := sid.unmarshal(req.StreamId()); err == nil {
					latency := s.latencyManager.LatencyFor(sid.path)
					req.SetLatency(latency, latency)
				}
			}

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
				latencyManager:      s.latencyManager,

				lossAlarmReporter:       s.LossAlarmReporter,
				lossAlarmEnable:         s.LossAlarmEnable,
				lossAlarmThresholdPct:   s.LossAlarmThresholdPct,
				lossSampleReporter:      s.LossSampleReporter,
				lossDisconnectEnable:    s.LossDisconnectEnable,
				lossDisconnectSec:       s.LossDisconnectSec,
				lossRecycleEnable:       s.LossRecycleEnable,
				lossRecycleThresholdPct: s.LossRecycleThresholdPct,
				lossRecycleSec:          s.LossRecycleSec,

				degradeManager:          s.DegradeManager,
				degradeEnable:           s.DegradeEnable,
				degradeRaisePct:         s.DegradeRaisePct,
				degradeLowerPct:         s.DegradeLowerPct,
				degradeRestartPct:       s.DegradeRestartPct,
				degradeObservationSec:   s.DegradeObservationSec,
				degradeSampleSec:        s.DegradeSampleSec,
				degradeRaiseLatencyStep: s.DegradeRaiseLatencyStep,
				degradeLowerLatencyStep: s.DegradeLowerLatencyStep,
				degradeLatencyMin:       s.DegradeLatencyMin,
				degradeLatencyMax:       s.DegradeLatencyMax,
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
