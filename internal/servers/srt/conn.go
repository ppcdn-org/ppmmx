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

	lossAlarmReporter       srtLossAlarmReporter
	lossAlarmEnable         bool
	lossAlarmThresholdPct   float64
	lossSampleReporter      srtLossSampleReporter
	lossDisconnectEnable    bool
	lossDisconnectSec       int
	lossRecycleEnable       bool
	lossRecycleThresholdPct float64
	lossRecycleSec          int

	degradeManager         *degrade.Manager
	degradeEnable          bool
	degradeRaisePct        float64
	degradeLowerPct        float64
	degradeObservationSec  int
	degradeSampleSec       int
	degradeRaiseLatencyStep time.Duration
	degradeLowerLatencyStep time.Duration
	degradeLatencyMin      time.Duration
	degradeLatencyMax      time.Duration

	// latencyManager feeds this connection's unrecovered drop rate into
	// the per-path adaptive latency tuner (see latency.go and
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

	// Debug measurement: if this publish is the reconnect that an adaptive-
	// latency raise forced (see runReceiveStatsSummary), log how long the path
	// had no publisher - i.e. the ingest interruption that forced reconnect
	// cost. AddPublisher has just succeeded, so media is about to flow again;
	// the gap from the MarkRaiseClose stamp to now is the dead-air window. Only
	// the reconnect after a raise has a pending stamp - a normal first publish
	// takes nothing and logs nothing.
	if c.latencyManager != nil {
		if gap, ok := c.latencyManager.TakeRaiseCloseGap(streamID.path, time.Now()); ok {
			c.Log(logger.Info, "SRT publish resumed on path %s after %v of ingest interruption (adaptive-latency raise forced reconnect)",
				streamID.path, gap.Round(time.Millisecond))
		}
	}

	// Publish session history (see publish_session_report.go): tells ppcenter
	// this SRT stream went live, and - via the deferred call - when and why
	// it stopped. Reported at the lifecycle boundaries rather than sampled,
	// since these two events *are* the record.
	c.reportPublishStart(streamID.path)
	defer c.reportPublishEnd(sconn, streamID.path)

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
	// Separate tracker for the loss-recycle tier: its threshold differs from
	// the alarm/disconnect one, and SustainedLossTracker keys its continuous-
	// over-threshold clock to a single threshold, so the tiers cannot share
	// an instance.
	var recycleTracker recvstats.SustainedLossTracker
	var st srt.Statistics
	sconn.Stats(&st)
	sampler.Sample(st.Accumulated.ByteRecv, st.Accumulated.PktRecv, st.Accumulated.PktRecvLoss, time.Now()) // seed baseline

	// SRT's ARQ counterpart to the WHIP hops' NACK counters: retrans is
	// packets ARQ recovered, drop is packets gosrt received but discarded
	// because they arrived too late to play (the application will never see
	// them), and dup is duplicate packets discarded (already ACKed or
	// already buffered). Neither drop nor dup is the same thing as loss
	// that was never recovered at all; see the unrecoverableLoss comment
	// below. Reporting all of them makes the SRT ingest hop and the WHIP
	// forward hops directly comparable, instead of only the raw loss rate
	// they already share.
	lastRetrans, lastDrop := st.Accumulated.PktRecvRetrans, st.Accumulated.PktRecvDrop
	lastDup := st.Accumulated.PktRecvDuplicate
	lastBelated := st.Accumulated.PktRecvBelated
	lastLoss := st.Accumulated.PktRecvLoss

	for {
		select {
		case <-ticker.C:
			sconn.Stats(&st)
			if snap, ok := sampler.Sample(
				st.Accumulated.ByteRecv, st.Accumulated.PktRecv, st.Accumulated.PktRecvLoss, time.Now(),
			); ok {
				// unrecoveredPct is the loss figure the alarm and the
				// disconnect guard act on, NOT snap.LossPct. snap.LossPct is
				// raw SRT loss, most of which ARQ retransmits away - alarming
				// on it pages operators for conditions SRT is designed to
				// absorb (measured here: ~5-10% raw loss against ~0.13%
				// actually unrecovered). Only packets lost and never
				// retransmitted affect a viewer, so that is what is reported.
				//
				// haveUnrecovered stays false on the rare tick where the
				// cumulative counters went backwards (socket recreated): the
				// deltas would be meaningless, so the tracker is simply not
				// fed rather than fed a fabricated zero that would resolve an
				// active alarm.
				var unrecoveredPct, recoverablePct float64
				haveUnrecovered := false
				latencyRaised := false
				if st.Accumulated.PktRecvRetrans >= lastRetrans && st.Accumulated.PktRecvDrop >= lastDrop &&
					st.Accumulated.PktRecvDuplicate >= lastDup && st.Accumulated.PktRecvLoss >= lastLoss {
					dRetrans := st.Accumulated.PktRecvRetrans - lastRetrans
					dDrop := st.Accumulated.PktRecvDrop - lastDrop
					dDup := st.Accumulated.PktRecvDuplicate - lastDup
					dLost := st.Accumulated.PktRecvLoss - lastLoss

					// RTT (tens of ms) is far shorter than the 60s sample
					// window, so a retransmit for a gap detected in this
					// window overwhelmingly lands within it too - dRetrans
					// is not a windowed replay of NAKs from a prior interval.
					unrecovered, pct := unrecoverableLossPct(dLost, dRetrans, snap.PacketsExpected)
					unrecoveredPct = pct
					// Recoverable is simply what is left of the raw rate once
					// the never-recovered part is subtracted - i.e. the share
					// ARQ did retransmit in time. Clamped at zero only to
					// absorb float noise; by construction it cannot exceed
					// the raw rate, so recoverable + unrecoverable == raw.
					recoverablePct = snap.LossPct - unrecoveredPct
					if recoverablePct < 0 {
						recoverablePct = 0
					}
					haveUnrecovered = true

					snap.Extra = fmt.Sprintf(" retrans=%d drop=%d dup=%d unrecoverableLoss=%d(%.2f%%)",
						dRetrans, dDrop, dDup, unrecovered, unrecoveredPct)

					// Feed the adaptive-latency tuner (see
					// docs/srt-adaptive-latency-design.md). Zero-traffic
					// intervals are never recorded: an idle path would
					// otherwise look like a low-drop event and trigger a
					// spurious latency reduction.
					//
					// Skipped when the unified degrade protocol is enabled:
					// that path tunes latency on its own degrade/recover
					// events (runDegradeSampling -> AdjustLatency), so
					// letting this 60s path also tune would double-adjust.
					if c.latencyManager != nil && snap.PacketsExpected > 0 && !c.degradeEnable {
						latencyRaised = c.latencyManager.Record(pathName, unrecoveredPct)
					}
				}
				lastRetrans, lastDrop, lastDup, lastLoss = st.Accumulated.PktRecvRetrans, st.Accumulated.PktRecvDrop, st.Accumulated.PktRecvDuplicate, st.Accumulated.PktRecvLoss

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

				if (c.lossAlarmEnable || c.lossDisconnectEnable) && haveUnrecovered {
					decision := lossTracker.Update(unrecoveredPct, c.lossAlarmThresholdPct, c.lossDisconnectEnable,
						time.Duration(c.lossDisconnectSec)*time.Second, time.Now())

					if c.lossAlarmEnable && decision.ShouldReport && c.lossAlarmReporter != nil {
						reporter, path := c.lossAlarmReporter, pathName
						pct, bitrateBps, sustainedSec := unrecoveredPct, snap.BitrateBps, int(decision.Sustained.Seconds())
						go func() {
							ctx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
							defer cancel()
							if err := reporter.ReportSRTLoss(ctx, path, pct, bitrateBps, sustainedSec); err != nil {
								c.Log(logger.Debug, "SRT loss alarm report failed: %v", err)
							}
						}()
					}

					if decision.ShouldDisconnect {
						c.Log(logger.Warn, "SRT unrecovered loss=%.2f%% sustained %s >= %ds, forcing disconnect so the publisher reconnects",
							unrecoveredPct, decision.Sustained.Round(time.Second), c.lossDisconnectSec)
						c.Close()
						return
					}
				}

				// Loss-recycle tier (see conf.SRTLossRecycleEnable): a second,
				// slower disconnect for chronic MILD loss - a low threshold
				// sustained far longer than lossDisconnectSec. Own tracker so it
				// never shares the alarm/disconnect clock. The disconnect tier
				// above returns on its first fire, so if both are enabled and it
				// trips first this is not reached that interval. The log keeps
				// the "SRT unrecovered loss=...forcing disconnect" shape so it
				// classifies as the same publish-reconnect health event.
				if c.lossRecycleEnable && haveUnrecovered {
					rd := recycleTracker.Update(unrecoveredPct, c.lossRecycleThresholdPct, true,
						time.Duration(c.lossRecycleSec)*time.Second, time.Now())
					if rd.ShouldDisconnect {
						c.Log(logger.Warn, "SRT unrecovered loss=%.2f%% mild but sustained %s >= %ds, forcing disconnect to recycle the connection so the publisher reconnects",
							unrecoveredPct, rd.Sustained.Round(time.Second), c.lossRecycleSec)
						c.Close()
						return
					}
				}

				// Persist the per-minute breakdown for the superadmin trend
				// view - unlike the alarm above, this is unconditional (every
				// interval, whatever the loss) so the trend has the healthy
				// minutes too, not just the bad ones.
				if c.lossSampleReporter != nil && haveUnrecovered {
					reporter, path := c.lossSampleReporter, pathName
					rec, unrec, bitrate := recoverablePct, unrecoveredPct, snap.BitrateBps
					minute := time.Now().UTC().Format("2006-01-02 15:04")
					go func() {
						ctx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
						defer cancel()
						if err := reporter.ReportSRTLossSample(ctx, path, rec, unrec, bitrate, minute); err != nil {
							c.Log(logger.Debug, "SRT loss sample report failed: %v", err)
						}
					}()
				}

				// A newly RAISED tuned latency only reaches the wire on a
				// fresh handshake - SRT fixes the receive/TSBPD delay at
				// connect time (see docs/srt-adaptive-latency-design.md) - so
				// force this publisher to reconnect and pick it up. Only raises
				// trigger this: a raise is fixing active unrecovered loss, so
				// the ~1s OBS reconnect pays for itself, whereas a lower would
				// interrupt a healthy stream just to trim delay and is left to
				// apply at the path's next natural reconnect. Same forced-
				// reconnect lever as the loss disconnect above (if that already
				// fired this interval it returned, so this never double-closes);
				// done after the stats line and loss sample so this interval is
				// still fully logged first.
				if latencyRaised {
					// Stamp the close time so the reconnecting publisher can
					// report how long the ingest was interrupted (see
					// runPublishReader). latencyManager is non-nil here - a
					// raise can only be reported after a Record call, which is
					// itself guarded by the nil check above.
					c.latencyManager.MarkRaiseClose(pathName, time.Now())
					c.Log(logger.Info, "SRT receive latency raised for path %s; forcing publisher reconnect to apply it", pathName)
					c.Close()
					return
				}
			}

		case <-done:
			return

		case <-c.ctx.Done():
			return
		}
	}
}

// unrecoverableAccumulator turns cumulative SRT (lost, retrans, received)
// counters into monotonically increasing cumulative (unrecoverable,
// expected) totals, so the degrade FSM can be fed an UNRECOVERABLE-loss
// rate instead of the raw SRT loss rate.
//
// Why not just pass PktRecvLoss straight through: raw loss counts every
// packet ARQ retransmits away, so on a healthy-but-lossy link (measured
// baseline ~5-10% raw, ~0.1% unrecoverable - see conf.go's
// SRTLossAlarmThresholdPct comment) triggering degrade on raw loss
// over-degrades and bottoms the ladder out on a condition SRT is designed
// to absorb. unrecoverable = lost - retrans, but that difference is NOT
// monotonic on its own: a gap detected in one 1s tick can be filled by a
// retransmission in a later one, so lost-retrans can dip while both inputs
// only rise, and degrade.State.Sample's reset detection requires the
// cumulative reading it is fed to only ever increase. Accumulating the
// per-tick deltas here keeps that guarantee.
type unrecoverableAccumulator struct {
	seeded      bool
	lastLoss    uint64
	lastRetrans uint64
	lastRecv    uint64

	unrecoverable uint64 // cumulative lost - recovered
	expected      uint64 // cumulative lost + received
}

// add folds one cumulative reading in and reports whether the caller should
// feed the resulting totals to the FSM. It returns false only for the very
// first reading and for a counter reset (a new SRT socket after a
// reconnect), in which case it just (re-)seeds the baseline; the
// accumulated totals deliberately carry across reconnects.
func (a *unrecoverableAccumulator) add(curLoss, curRetrans, curRecv uint64) bool {
	if !a.seeded {
		a.lastLoss, a.lastRetrans, a.lastRecv, a.seeded = curLoss, curRetrans, curRecv, true
		return true
	}
	if curLoss < a.lastLoss || curRetrans < a.lastRetrans || curRecv < a.lastRecv {
		a.lastLoss, a.lastRetrans, a.lastRecv = curLoss, curRetrans, curRecv
		return false
	}
	dLost := curLoss - a.lastLoss
	dRetrans := curRetrans - a.lastRetrans
	dRecv := curRecv - a.lastRecv
	if dLost > dRetrans {
		a.unrecoverable += dLost - dRetrans
	}
	a.expected += dLost + dRecv
	a.lastLoss, a.lastRetrans, a.lastRecv = curLoss, curRetrans, curRecv
	return true
}

// runDegradeSampling periodically feeds this SRT publish connection's
// UNRECOVERABLE packet-loss rate into the path's degrade FSM (see
// internal/degrade and docs/design/publish-degrade-protocol.zh-CN.md), at
// the degrade package's default cadence (6s). On a degrade action, it also
// adjusts the per-path SRT receiver latency (raise: force reconnect; lower:
// opportunistic). Unrecoverable (lost minus what ARQ retransmitted), not raw
// loss: raw loss is mostly recovered on healthy links and would over-degrade
// (see unrecoverableAccumulator). videoLayers is this connection's multiplexed
// video track count (see runPublishReader's mpegts.ValidateVideoTracks call),
// SRT's analog of WHIP's inbound Simulcast track count.
func (c *conn) runDegradeSampling(sconn srt.Conn, pathName string, videoLayers int, done <-chan struct{}) {
	c.degradeManager.ObserveSessionLayers(pathName, videoLayers)

	ticker := time.NewTicker(degrade.SamplePeriod(c.degradeSampleSec))
	defer ticker.Stop()

	var st srt.Statistics
	var acc unrecoverableAccumulator
	for {
		select {
		case <-ticker.C:
			sconn.Stats(&st)
			if acc.add(st.Accumulated.PktRecvLoss, st.Accumulated.PktRecvRetrans, st.Accumulated.PktRecv) {
				changed := c.degradeManager.RecordSample(pathName, acc.unrecoverable,
					acc.expected, degrade.Thresholds{
						RaisePct:       c.degradeRaisePct,
						LowerPct:       c.degradeLowerPct,
						ObservationSec: c.degradeObservationSec,
					})
				// On degrade/recover, also adjust SRT receiver latency.
				if changed != degrade.ActionNone && c.latencyManager != nil {
					raise := changed == degrade.ActionDegrade
					step := c.degradeRaiseLatencyStep
					if !raise {
						step = c.degradeLowerLatencyStep
					}
					if raised := c.latencyManager.AdjustLatency(pathName, raise, step,
						c.degradeLatencyMin, c.degradeLatencyMax); raised {
						c.latencyManager.MarkRaiseClose(pathName, time.Now())
						c.Log(logger.Info, "[degrade] path=%s raise-triggered latency raise; forcing reconnect", pathName)
						c.Close()
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
