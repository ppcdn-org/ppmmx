package srt

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	srt "github.com/datarhei/gosrt"
	"github.com/google/uuid"

	"github.com/bluenviron/mediamtx/internal/auth"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/degrade"
	"github.com/bluenviron/mediamtx/internal/errordumper"
	"github.com/bluenviron/mediamtx/internal/externalcmd"
	"github.com/bluenviron/mediamtx/internal/hooks"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/protocols/mpegts"
	"github.com/bluenviron/mediamtx/internal/recvstats"
	"github.com/bluenviron/mediamtx/internal/stream"
)

func srtCheckPassphrase(connReq srt.ConnRequest, passphrase string) error {
	if passphrase == "" {
		return nil
	}

	if !connReq.IsEncrypted() {
		return fmt.Errorf("connection is encrypted, but not passphrase is defined in configuration")
	}

	err := connReq.SetPassphrase(passphrase)
	if err != nil {
		return fmt.Errorf("invalid passphrase")
	}

	return nil
}

type conn struct {
	parentCtx           context.Context
	rtspAddress         string
	readTimeout         conf.Duration
	writeTimeout        conf.Duration
	udpMaxPayloadSize   int
	connReq             srt.ConnRequest
	runOnConnect        string
	runOnConnectRestart bool
	runOnDisconnect     string
	wg                  *sync.WaitGroup
	externalCmdPool     *externalcmd.Pool
	pathManager         serverPathManager
	publishAuthKey      string
	publishTokenReq     bool
	parent              *Server

	lossAlarmReporter     srtLossAlarmReporter
	lossAlarmEnable       bool
	lossAlarmThresholdPct float64
	lossDisconnectEnable  bool
	lossDisconnectSec     int

	degradeManager        *degrade.Manager
	degradeEnable         bool
	degradeInstantLossPct float64
	degradeAvgLossPct     float64
	recoverInstantLossPct float64
	recoverAvgLossPct     float64
	degradeObservationSec int

	// latencyManager feeds this connection's unrecovered drop rate into
	// the per-path adaptive latency evaluator (see latency.go and
	// docs/srt-adaptive-latency-design.md). Nil when SRTLatencyAutoTune
	// is disabled.
	latencyManager *latencyManager

	ctx       context.Context
	ctxCancel func()
	created   time.Time
	uuid      uuid.UUID
	mutex     sync.RWMutex
	state     defs.APISRTConnState
	pathName  string
	query     string
	user      string
	sconn     srt.Conn
	reader    *stream.Reader
}

func (c *conn) initialize() {
	c.ctx, c.ctxCancel = context.WithCancel(c.parentCtx)

	c.created = time.Now()
	c.uuid = uuid.New()
	c.state = defs.APISRTConnStateIdle

	c.Log(logger.Info, "opened")

	c.wg.Add(1)
	go c.run()
}

func (c *conn) Close() {
	c.ctxCancel()
}

// Log implements logger.Writer.
func (c *conn) Log(level logger.Level, format string, args ...any) {
	c.parent.Log(level, "[conn %v] "+format, append([]any{c.connReq.RemoteAddr()}, args...)...)
}

func (c *conn) ip() net.IP {
	return c.connReq.RemoteAddr().(*net.UDPAddr).IP
}

func (c *conn) run() { //nolint:dupl
	defer c.wg.Done()

	onDisconnectHook := hooks.OnConnect(hooks.OnConnectParams{
		Logger:              c,
		ExternalCmdPool:     c.externalCmdPool,
		RunOnConnect:        c.runOnConnect,
		RunOnConnectRestart: c.runOnConnectRestart,
		RunOnDisconnect:     c.runOnDisconnect,
		RTSPAddress:         c.rtspAddress,
		Desc:                *c.APIReaderDescribe(),
	})
	defer onDisconnectHook()

	err := c.runInner()

	c.ctxCancel()

	c.parent.closeConn(c)

	c.Log(logger.Info, "closed: %v", err)
}

func (c *conn) runInner() error {
	var streamID streamID
	err := streamID.unmarshal(c.connReq.StreamId())
	if err != nil {
		c.connReq.Reject(srt.REJ_PEER)
		return fmt.Errorf("invalid stream ID '%s': %w", c.connReq.StreamId(), err)
	}

	if streamID.mode == streamIDModePublish {
		return c.runPublish(&streamID)
	}
	return c.runRead(&streamID)
}

func (c *conn) runPublish(streamID *streamID) error {
	// Checked before FindPathConf so an unauthorized publish is rejected
	// without touching path configuration or hooks.
	switch err := c.checkPublishToken(streamID); {
	case err == nil:
	case errors.Is(err, errTokenUncheckable):
		c.Log(logger.Warn, "%v", err)
	default:
		c.connReq.Reject(srt.REJ_PEER)
		return err
	}

	res, err := c.pathManager.FindPathConf(defs.PathFindPathConfReq{
		AccessRequest: defs.PathAccessRequest{
			Name:    streamID.path,
			Query:   streamID.query,
			Publish: true,
			Proto:   auth.ProtocolSRT,
			ID:      &c.uuid,
			Credentials: &auth.Credentials{
				User: streamID.user,
				Pass: streamID.pass,
			},
			IP: c.ip(),
		},
	})
	if err != nil {
		if terr, ok := errors.AsType[*auth.Error](err); ok {
			c.connReq.Reject(srt.REJ_PEER)
			return terr
		}
		c.connReq.Reject(srt.REJ_PEER)
		return err
	}

	c.mutex.Lock()
	c.user = res.User
	c.mutex.Unlock()

	err = srtCheckPassphrase(c.connReq, res.Conf.SRTPublishPassphrase)
	if err != nil {
		c.connReq.Reject(srt.REJ_PEER)
		return err
	}

	sconn, err := c.connReq.Accept()
	if err != nil {
		return err
	}

	readerErr := make(chan error)
	go func() {
		readerErr <- c.runPublishReader(sconn, streamID, res.Conf)
	}()

	select {
	case err = <-readerErr:
		sconn.Close()
		return err

	case <-c.ctx.Done():
		sconn.Close()
		<-readerErr
		return errors.New("terminated")
	}
}

func (c *conn) runPublishReader(sconn srt.Conn, streamID *streamID, pathConf *conf.Path) error {
	sconn.SetReadDeadline(time.Now().Add(time.Duration(c.readTimeout)))
	r := &mpegts.EnhancedReader{R: sconn}
	err := r.Initialize()
	if err != nil {
		return err
	}

	videoTracks, err := mpegts.ValidateVideoTracks(r.Tracks())
	if err != nil {
		return err
	}

	// SRT has no SDP offer/answer, so the codec only becomes known once the
	// PMT arrives - this is the earliest a path/codec mismatch can be
	// detected, which is why it surfaces as "connected, then dropped"
	// rather than a rejected handshake.
	if err := checkTracksMatchPathCodec(streamID.path, videoTracks); err != nil {
		return err
	}

	if len(videoTracks) > 1 {
		c.Log(logger.Info, "SRT publish contains %d %s video tracks; treating them as a simulcast ladder (highest quality first)",
			len(videoTracks), mpegts.VideoCodecName(videoTracks[0].Codec))
		for i, track := range videoTracks {
			elementaryOrder := 0
			for j, candidate := range r.Tracks() {
				if candidate == track {
					elementaryOrder = j + 1
					break
				}
			}
			c.Log(logger.Info, "simulcast layer %d: MPEG-TS elementary stream order %d, PID %d", i, elementaryOrder, track.PID)
		}
	}

	decodeErrors := &errordumper.Dumper{
		OnReport: func(val uint64, last error) {
			if val == 1 {
				c.Log(logger.Warn, "decode error: %v", last)
			} else {
				c.Log(logger.Warn, "%d decode errors, last was: %v", val, last)
			}
		},
	}

	decodeErrors.Start()
	defer decodeErrors.Stop()

	r.OnDecodeError(func(err error) {
		decodeErrors.Add(err)
	})

	var subStream *stream.SubStream

	medias, err := mpegts.ToStream(r, &subStream, c)
	if err != nil {
		return err
	}

	res, err := c.pathManager.AddPublisher(defs.PathAddPublisherReq{
		Author:        c,
		Desc:          &description.Session{Medias: medias},
		UseRTPPackets: false,
		ReplaceNTP:    true,
		ConfToCompare: pathConf,
		AccessRequest: defs.PathAccessRequest{
			Name:     streamID.path,
			Query:    streamID.query,
			Publish:  true,
			SkipAuth: true,
		},
	})
	if err != nil {
		return err
	}

	defer res.Path.RemovePublisher(defs.PathRemovePublisherReq{Author: c})

	subStream = res.SubStream

	c.mutex.Lock()
	c.state = defs.APISRTConnStatePublish
	c.pathName = streamID.path
	c.query = streamID.query
	c.sconn = sconn
	c.mutex.Unlock()

	// log ingest receive bitrate + packet-loss every recvstats.Interval for
	// the life of this publish (stops when the read loop below returns).
	statsDone := make(chan struct{})
	defer close(statsDone)
	go c.runReceiveStatsSummary(sconn, streamID.path, statsDone)
	if c.degradeEnable {
		go c.runDegradeSampling(sconn, streamID.path, len(videoTracks), statsDone)
	}

	for {
		err = r.Read()
		if err != nil {
			return err
		}
	}
}

// unrecoverableLossPct computes the share of packets this interval expected
// to receive (packetsExpected, recvstats.Snapshot's own LossPct denominator)
// that gosrt's ARQ never recovered: NAK-detected gaps (dLost) minus the ones
// a retransmit actually filled (dRetrans). See the call site in
// runReceiveStatsSummary for why this - not PktRecvDrop - is the right
// numerator: PktDrop only covers packets that DID arrive (too late, or a
// second time); a sequence number that never arrives at all isn't added to
// any counter except PktLoss, so subtracting what ARQ recovered from it is
// the only way to surface real, permanent loss.
func unrecoverableLossPct(dLost, dRetrans, packetsExpected uint64) (unrecovered uint64, pct float64) {
	if dLost > dRetrans {
		unrecovered = dLost - dRetrans
	}
	if packetsExpected > 0 {
		pct = float64(unrecovered) / float64(packetsExpected) * 100
	}
	return unrecovered, pct
}

// runReceiveStatsSummary periodically logs this SRT publish connection's
// receive bitrate and packet-loss rate, computed from the SRT socket's
// accumulated byte/packet counters (same format as every other ingest
// protocol - see recvstats).
func (c *conn) runReceiveStatsSummary(sconn srt.Conn, pathName string, done <-chan struct{}) {
	ticker := time.NewTicker(recvstats.Interval)
	defer ticker.Stop()

	var sampler recvstats.Sampler
	var lossTracker recvstats.SustainedLossTracker
	var st srt.Statistics
	sconn.Stats(&st)
	sampler.Sample(st.Accumulated.ByteRecv, st.Accumulated.PktRecv, st.Accumulated.PktRecvLoss, time.Now()) // seed baseline

	// SRT's ARQ counterpart to the WHIP hops' NACK counters: retrans is
	// packets ARQ recovered, drop is packets gosrt received but discarded
	// (too late/duplicate/already-ACKed - not the same thing as loss that
	// was never recovered at all; see the unrecoverableLoss comment below).
	// Reporting all of them makes the SRT ingest hop and the WHIP forward
	// hops directly comparable, instead of only the raw loss rate they
	// already share.
	lastRetrans, lastDrop := st.Accumulated.PktRecvRetrans, st.Accumulated.PktRecvDrop
	lastBelated := st.Accumulated.PktRecvBelated
	lastLoss := st.Accumulated.PktRecvLoss

	for {
		select {
		case <-ticker.C:
			sconn.Stats(&st)
			if snap, ok := sampler.Sample(
				st.Accumulated.ByteRecv, st.Accumulated.PktRecv, st.Accumulated.PktRecvLoss, time.Now(),
			); ok {
				if st.Accumulated.PktRecvRetrans >= lastRetrans && st.Accumulated.PktRecvDrop >= lastDrop &&
					st.Accumulated.PktRecvLoss >= lastLoss {
					dRetrans := st.Accumulated.PktRecvRetrans - lastRetrans
					dDrop := st.Accumulated.PktRecvDrop - lastDrop
					dLost := st.Accumulated.PktRecvLoss - lastLoss

					// RTT (tens of ms) is far shorter than the 60s sample
					// window, so a retransmit for a gap detected in this
					// window overwhelmingly lands within it too - dRetrans
					// is not a windowed replay of NAKs from a prior interval.
					unrecovered, unrecoveredPct := unrecoverableLossPct(dLost, dRetrans, snap.PacketsExpected)

					snap.Extra = fmt.Sprintf(" retrans=%d drop=%d unrecoverableLoss=%d(%.2f%%)",
						dRetrans, dDrop, unrecovered, unrecoveredPct)

					// Feed the adaptive-latency evaluator (see
					// docs/srt-adaptive-latency-design.md). Zero-traffic
					// intervals are never recorded: an idle path would
					// otherwise pull its p95 toward zero and trigger a
					// spurious latency reduction.
					if c.latencyManager != nil && snap.PacketsExpected > 0 {
						c.latencyManager.Record(pathName, unrecoveredPct)
					}
				}
				lastRetrans, lastDrop, lastLoss = st.Accumulated.PktRecvRetrans, st.Accumulated.PktRecvDrop, st.Accumulated.PktRecvLoss

				// Link diagnostics, to tell a bandwidth ceiling apart from
				// the other things that produce the same loss figure:
				// linkCapacity is SRT's own estimate of what the path can
				// carry (loss at a rate well under it is not congestion),
				// rtt and its spread show queueing, belated/reorder count
				// packets that arrived too late to use rather than never
				// arriving at all, and recvBuf shows whether the receiver
				// itself is falling behind.
				snap.Extra += fmt.Sprintf(
					" linkCapacity=%.1fMbps recvRate=%.1fMbps rtt=%.1fms belated=%d reorderTol=%d recvBuf=%dms",
					st.Instantaneous.MbpsLinkCapacity, st.Instantaneous.MbpsRecvRate,
					st.Instantaneous.MsRTT,
					st.Accumulated.PktRecvBelated-lastBelated,
					st.Instantaneous.PktReorderTolerance,
					st.Instantaneous.MsRecvBuf)
				lastBelated = st.Accumulated.PktRecvBelated

				c.Log(logger.Info, "%s", snap.LogLine("srt", pathName))

				if c.lossAlarmEnable || c.lossDisconnectEnable {
					decision := lossTracker.Update(snap.LossPct, c.lossAlarmThresholdPct, c.lossDisconnectEnable,
						time.Duration(c.lossDisconnectSec)*time.Second, time.Now())

					if c.lossAlarmEnable && decision.ShouldReport && c.lossAlarmReporter != nil {
						reporter, path := c.lossAlarmReporter, pathName
						lossPct, bitrateBps, sustainedSec := snap.LossPct, snap.BitrateBps, int(decision.Sustained.Seconds())
						go func() {
							ctx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
							defer cancel()
							if err := reporter.ReportSRTLoss(ctx, path, lossPct, bitrateBps, sustainedSec); err != nil {
								c.Log(logger.Debug, "SRT loss alarm report failed: %v", err)
							}
						}()
					}

					if decision.ShouldDisconnect {
						c.Log(logger.Warn, "SRT loss=%.2f%% sustained %s >= %ds, forcing disconnect so the publisher reconnects",
							snap.LossPct, decision.Sustained.Round(time.Second), c.lossDisconnectSec)
						c.Close()
						return
					}
				}
			}

		case <-done:
			return

		case <-c.ctx.Done():
			return
		}
	}
}

// runDegradeSampling periodically feeds this SRT publish connection's
// cumulative packet loss/received counters into the path's degrade FSM
// (see internal/degrade and docs/obs-mmx-degrade-protocol.md), at the same
// 1-second cadence WHIP's own equivalent uses (degrade.SampleInterval) -
// the FSM's trailing average window is calibrated assuming every caller
// samples at that exact cadence. videoLayers is this connection's
// multiplexed video track count (see runPublishReader's
// mpegts.ValidateVideoTracks call), SRT's analog of WHIP's inbound
// Simulcast track count.
func (c *conn) runDegradeSampling(sconn srt.Conn, pathName string, videoLayers int, done <-chan struct{}) {
	c.degradeManager.ObserveSessionLayers(pathName, videoLayers)

	ticker := time.NewTicker(degrade.SampleInterval)
	defer ticker.Stop()

	var st srt.Statistics
	for {
		select {
		case <-ticker.C:
			sconn.Stats(&st)
			c.degradeManager.RecordSample(pathName, st.Accumulated.PktRecvLoss, st.Accumulated.PktRecv, degrade.Thresholds{
				DegradeInstantLossPct: c.degradeInstantLossPct,
				DegradeAvgLossPct:     c.degradeAvgLossPct,
				RecoverInstantLossPct: c.recoverInstantLossPct,
				RecoverAvgLossPct:     c.recoverAvgLossPct,
				ObservationSec:        c.degradeObservationSec,
			})

		case <-done:
			return

		case <-c.ctx.Done():
			return
		}
	}
}

func (c *conn) runRead(streamID *streamID) error {
	res, err := c.pathManager.AddReader(defs.PathAddReaderReq{
		Author: c,
		AccessRequest: defs.PathAccessRequest{
			Name:  streamID.path,
			Query: streamID.query,
			Proto: auth.ProtocolSRT,
			ID:    &c.uuid,
			Credentials: &auth.Credentials{
				User: streamID.user,
				Pass: streamID.pass,
			},
			IP: c.ip(),
		},
	})
	if err != nil {
		if terr, ok := errors.AsType[*auth.Error](err); ok {
			c.connReq.Reject(srt.REJ_PEER)
			return terr
		}
		c.connReq.Reject(srt.REJ_PEER)
		return err
	}

	defer res.Path.RemoveReader(defs.PathRemoveReaderReq{Author: c})

	err = srtCheckPassphrase(c.connReq, res.Path.SafeConf().SRTReadPassphrase)
	if err != nil {
		c.connReq.Reject(srt.REJ_PEER)
		return err
	}

	sconn, err := c.connReq.Accept()
	if err != nil {
		return err
	}
	defer sconn.Close()

	bw := bufio.NewWriterSize(sconn, srtMaxPayloadSize(c.udpMaxPayloadSize))

	r := &stream.Reader{Parent: c}

	err = mpegts.FromStream(res.Stream.OrigDesc, r, bw, sconn, time.Duration(c.writeTimeout))
	if err != nil {
		return err
	}

	c.mutex.Lock()
	c.state = defs.APISRTConnStateRead
	c.pathName = streamID.path
	c.query = streamID.query
	c.user = res.User
	c.sconn = sconn
	c.mutex.Unlock()

	c.Log(logger.Info, "is reading from path '%s', %s",
		res.Path.Name(), defs.FormatsInfo(r.Formats()))

	onUnreadHook := hooks.OnRead(hooks.OnReadParams{
		Logger:          c,
		ExternalCmdPool: c.externalCmdPool,
		Conf:            res.Path.SafeConf(),
		ExternalCmdEnv:  res.Path.ExternalCmdEnv(),
		Reader:          *c.APIReaderDescribe(),
		Query:           streamID.query,
	})
	defer onUnreadHook()

	// disable read deadline
	sconn.SetReadDeadline(time.Time{})

	res.Stream.AddReader(r)
	defer res.Stream.RemoveReader(r)

	c.mutex.Lock()
	c.reader = r
	c.mutex.Unlock()

	select {
	case <-c.ctx.Done():
		return fmt.Errorf("terminated")

	case err = <-r.Error():
		return err
	}
}

// APIReaderDescribe implements reader.
func (c *conn) APIReaderDescribe() *defs.APIPathReader {
	return &defs.APIPathReader{
		Type: defs.APIPathReaderTypeSRTConn,
		ID:   c.uuid.String(),
	}
}

// APISourceDescribe implements source.
func (c *conn) APISourceDescribe() *defs.APIPathSource {
	return &defs.APIPathSource{
		Type: defs.APIPathSourceTypeSRTConn,
		ID:   c.uuid.String(),
	}
}

func (c *conn) apiItem() *defs.APISRTConn {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	item := &defs.APISRTConn{
		ID:         c.uuid,
		Created:    c.created,
		RemoteAddr: c.connReq.RemoteAddr().String(),
		State:      c.state,
		Path:       c.pathName,
		Query:      c.query,
		User:       c.user,
	}

	if c.sconn != nil {
		var s srt.Statistics
		c.sconn.Stats(&s)

		item.PacketsSent = s.Accumulated.PktSent
		item.PacketsReceived = s.Accumulated.PktRecv
		item.PacketsSentUnique = s.Accumulated.PktSentUnique
		item.PacketsReceivedUnique = s.Accumulated.PktRecvUnique
		item.PacketsSendLoss = s.Accumulated.PktSendLoss
		item.PacketsReceivedLoss = s.Accumulated.PktRecvLoss
		item.PacketsRetrans = s.Accumulated.PktRetrans
		item.PacketsReceivedRetrans = s.Accumulated.PktRecvRetrans
		item.PacketsSentACK = s.Accumulated.PktSentACK
		item.PacketsReceivedACK = s.Accumulated.PktRecvACK
		item.PacketsSentNAK = s.Accumulated.PktSentNAK
		item.PacketsReceivedNAK = s.Accumulated.PktRecvNAK
		item.PacketsSentKM = s.Accumulated.PktSentKM
		item.PacketsReceivedKM = s.Accumulated.PktRecvKM
		item.UsSndDuration = s.Accumulated.UsSndDuration
		item.PacketsReceivedBelated = s.Accumulated.PktRecvBelated
		item.PacketsSendDrop = s.Accumulated.PktSendDrop
		item.PacketsReceivedDrop = s.Accumulated.PktRecvDrop
		item.PacketsReceivedUndecrypt = s.Accumulated.PktRecvUndecrypt
		item.BytesSent = s.Accumulated.ByteSent
		item.BytesReceived = s.Accumulated.ByteRecv
		item.BytesSentUnique = s.Accumulated.ByteSentUnique
		item.BytesReceivedUnique = s.Accumulated.ByteRecvUnique
		item.BytesReceivedLoss = s.Accumulated.ByteRecvLoss
		item.BytesRetrans = s.Accumulated.ByteRetrans
		item.BytesReceivedRetrans = s.Accumulated.ByteRecvRetrans
		item.BytesReceivedBelated = s.Accumulated.ByteRecvBelated
		item.BytesSendDrop = s.Accumulated.ByteSendDrop
		item.BytesReceivedDrop = s.Accumulated.ByteRecvDrop
		item.BytesReceivedUndecrypt = s.Accumulated.ByteRecvUndecrypt
		item.UsPacketsSendPeriod = s.Instantaneous.UsPktSendPeriod
		item.PacketsFlowWindow = s.Instantaneous.PktFlowWindow
		item.PacketsFlightSize = s.Instantaneous.PktFlightSize
		item.MsRTT = s.Instantaneous.MsRTT
		item.MbpsSendRate = s.Instantaneous.MbpsSentRate
		item.MbpsReceiveRate = s.Instantaneous.MbpsRecvRate
		item.MbpsLinkCapacity = s.Instantaneous.MbpsLinkCapacity
		item.BytesAvailSendBuf = s.Instantaneous.ByteAvailSendBuf
		item.BytesAvailReceiveBuf = s.Instantaneous.ByteAvailRecvBuf
		item.MbpsMaxBW = s.Instantaneous.MbpsMaxBW
		item.ByteMSS = s.Instantaneous.ByteMSS
		item.PacketsSendBuf = s.Instantaneous.PktSendBuf
		item.BytesSendBuf = s.Instantaneous.ByteSendBuf
		item.MsSendBuf = s.Instantaneous.MsSendBuf
		item.MsSendTsbPdDelay = s.Instantaneous.MsSendTsbPdDelay
		item.PacketsReceiveBuf = s.Instantaneous.PktRecvBuf
		item.BytesReceiveBuf = s.Instantaneous.ByteRecvBuf
		item.MsReceiveBuf = s.Instantaneous.MsRecvBuf
		item.MsReceiveTsbPdDelay = s.Instantaneous.MsRecvTsbPdDelay
		item.PacketsReorderTolerance = s.Instantaneous.PktReorderTolerance
		item.PacketsReceivedAvgBelatedTime = s.Instantaneous.PktRecvAvgBelatedTime
		item.PacketsSendLossRate = s.Instantaneous.PktSendLossRate
		item.PacketsReceivedLossRate = s.Instantaneous.PktRecvLossRate
	}

	if c.reader != nil {
		item.OutboundFramesDiscarded = c.reader.OutboundFramesDiscarded()
	}

	return item
}

// RequestKeyFrame implements defs.Publisher. SRT does not support PLI.
func (c *conn) RequestKeyFrame() error { return nil }
