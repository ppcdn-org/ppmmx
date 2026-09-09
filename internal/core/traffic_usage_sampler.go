package core

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/mmxcontrol"
)

// trafficUsageSampleInterval is how often the sampler polls
// APISessionsList() and reports deltas. Independent of mmxHeartbeatInterval
// (the node registration heartbeat) - usage reporting has its own cadence
// since under-sampling only delays revenue recognition by a few seconds,
// not correctness, so it doesn't need to track the control-plane's tighter
// liveness requirements.
const trafficUsageSampleInterval = 30 * time.Second

// trafficUsageReporter is the subset of TrafficUsageClient the sampler
// needs - narrowed so tests can substitute a fake without a real HTTP round
// trip.
type trafficUsageReporter interface {
	Report(ctx context.Context, report mmxcontrol.TrafficUsageReport) error
}

// trafficUsageServer is the subset of defs.APIWebRTCServer the sampler
// polls. Read-only WHEP sessions (state "read") are what's billed - see
// docs/design/ppcdn-billing-and-traffic-monitoring.zh-CN.md §2.1's
// "no upstream fee" decision, so publish sessions are skipped entirely
// regardless of their own OutboundBytes (which for a publish session's
// PeerConnection is typically 0/near-0 RTCP feedback traffic anyway, not
// the media itself).
type trafficUsageServer interface {
	APISessionsList() (*defs.APIWebRTCSessionList, error)
}

// TrafficUsageSampler periodically diffs each active WHEP session's
// cumulative OutboundBytes (from pc.Stats(), surfaced via
// APISessionsList) against its own last-seen value and reports the
// incremental delta to ppcenter, keyed by the appId parsed from the
// session's path. This is the byte-level metering input the usage-based
// billing model (docs/design/ppcdn-billing-and-traffic-monitoring.zh-CN.md
// §2) needs and the old flat-fee model never collected.
type TrafficUsageSampler struct {
	Server   trafficUsageServer
	Reporter trafficUsageReporter
	Parent   logger.Writer

	ctx       context.Context
	ctxCancel func()
	done      chan struct{}

	mu       sync.Mutex
	lastSeen map[string]uint64 // session UUID (string) -> last-reported cumulative OutboundBytes
}

// Initialize starts the sampler's background loop.
func (s *TrafficUsageSampler) Initialize() {
	s.ctx, s.ctxCancel = context.WithCancel(context.Background())
	s.done = make(chan struct{})
	s.lastSeen = make(map[string]uint64)
	go s.run()
}

// Close stops the sampler and waits for its goroutine to exit.
func (s *TrafficUsageSampler) Close() {
	s.ctxCancel()
	<-s.done
}

// Log implements logger.Writer.
func (s *TrafficUsageSampler) Log(level logger.Level, format string, args ...any) {
	s.Parent.Log(level, "[traffic usage] "+format, args...)
}

func (s *TrafficUsageSampler) run() {
	defer close(s.done)

	ticker := time.NewTicker(trafficUsageSampleInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.sampleOnce()
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *TrafficUsageSampler) sampleOnce() {
	list, err := s.Server.APISessionsList()
	if err != nil {
		s.Log(logger.Warn, "session list failed: %v", err)
		return
	}

	usageDate := time.Now().UTC().Format("2006-01-02")
	seenThisRound := make(map[string]struct{}, len(list.Items))

	s.mu.Lock()
	for _, item := range list.Items {
		if item.State != defs.APIWebRTCSessionStateRead {
			continue
		}
		id := item.ID.String()
		seenThisRound[id] = struct{}{}

		appID := appIDFromPath(item.Path)
		if appID == "" {
			continue
		}

		prev, hadPrev := s.lastSeen[id]
		current := item.OutboundBytes
		s.lastSeen[id] = current

		var delta uint64
		switch {
		case !hadPrev:
			// First sample of this session: nothing to diff against, so
			// the entire cumulative value observed so far is the delta -
			// bytes already sent between session start and this first
			// tick are real usage, not to be silently dropped.
			delta = current
		case current > prev:
			delta = current - prev
		default:
			// current <= prev: OutboundBytes went flat or backwards
			// (idle session, or a counter reset mid-life) - not a valid
			// delta, resync lastSeen above and skip reporting this round.
			continue
		}
		if delta == 0 {
			continue
		}

		report := mmxcontrol.TrafficUsageReport{
			AppID:                appID,
			UsageDate:            usageDate,
			DownstreamDeltaBytes: int64(delta),
		}
		// Reported synchronously within the lock-held loop is intentionally
		// avoided below - collect first, report after unlocking, so a slow
		// or failing HTTP call never blocks the next tick's map access.
		s.reportAfterUnlock(report)
	}

	// Sessions that closed since the last round leave the map growing
	// forever if never pruned - drop anything not seen this round.
	for id := range s.lastSeen {
		if _, ok := seenThisRound[id]; !ok {
			delete(s.lastSeen, id)
		}
	}
	s.mu.Unlock()
}

// reportAfterUnlock queues a report to be sent once sampleOnce releases its
// lock. Kept synchronous but outside the critical section: reports for one
// tick can safely run sequentially since trafficUsageSampleInterval is far
// longer than a single HTTP round trip, and this avoids introducing
// unbounded goroutine fan-out per sample tick.
func (s *TrafficUsageSampler) reportAfterUnlock(report mmxcontrol.TrafficUsageReport) {
	go func() {
		ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
		defer cancel()
		if err := s.Reporter.Report(ctx, report); err != nil {
			s.Log(logger.Debug, "report failed: appId=%s usageDate=%s deltaBytes=%d err=%v",
				report.AppID, report.UsageDate, report.DownstreamDeltaBytes, err)
		}
	}()
}

// appIDFromPath extracts the leading appId segment from a WHEP session's
// path (format "{appId}/{streamName}[/{codec}]" - see
// docs/design/whip-hevc-h264-multitrack-simulcast-design.zh-CN.md), the
// same convention BillingManager.extractAppID already relies on
// server-side in ppcenter.
func appIDFromPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	parts := strings.SplitN(path, "/", 2)
	return strings.TrimSpace(parts[0])
}
