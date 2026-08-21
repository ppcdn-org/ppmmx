package webrtc

import (
	"sync"
	"time"
)

// publishStatsStaleGap: a gap between one WHIP publish session ending and
// the next one starting on the same path bigger than this is treated as
// "streaming actually stopped for a while", not a quick reconnect - the
// next session starts a brand new streaming period (reconnect count and
// streaming-duration clock both reset). This is unrelated to the degrade
// protocol (see degrade.go): it works regardless of webrtcDegradeEnable,
// and only exists to give operators plain visibility into how often a
// publisher (typically OBS) has had to reconnect during a stream, e.g. due
// to real-world network loss too brief/mild to trip the degrade FSM's 60s
// observation window.
const publishStatsStaleGap = 30 * time.Second

// publishSessionStartInfo is returned by publishReconnectStats.sessionStarted
// so the caller (a session) can log a one-off "this is a reconnect" message
// immediately, in addition to the periodic summary.
type publishSessionStartInfo struct {
	isReconnect    bool
	reconnectCount int
	streamingSince time.Time
}

// publishReconnectStats tracks, per path, how many times a WHIP publish
// session has reconnected during a single continuous "streaming period"
// (see publishStatsStaleGap) and since when that period has been running.
// Owned by Server, looked up/created by path name; outlives individual
// sessions for the same reason degradeState does (see degrade.go): a
// reconnect creates a brand new session, and the count/clock must survive
// that.
type publishReconnectStats struct {
	mu             sync.Mutex
	periodStart    time.Time // zero: no streaming period observed yet
	lastSessionEnd time.Time // zero while a session is currently active
	reconnectCount int
}

// sessionStarted is called when a WHIP publish session for this path
// becomes the active publisher (AddPublisher succeeded). Determines
// whether this continues the current streaming period (quick reconnect)
// or starts a fresh one (first-ever connect, or reconnecting after a gap
// longer than publishStatsStaleGap).
func (p *publishReconnectStats) sessionStarted() publishSessionStartInfo {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	isReconnect := !p.periodStart.IsZero() &&
		!p.lastSessionEnd.IsZero() &&
		now.Sub(p.lastSessionEnd) <= publishStatsStaleGap

	if isReconnect {
		p.reconnectCount++
	} else {
		p.periodStart = now
		p.reconnectCount = 0
	}
	p.lastSessionEnd = time.Time{} // mark active

	return publishSessionStartInfo{
		isReconnect:    isReconnect,
		reconnectCount: p.reconnectCount,
		streamingSince: p.periodStart,
	}
}

// sessionEnded records when a WHIP publish session for this path stopped,
// so the next sessionStarted call can tell a quick reconnect from a fresh
// streaming period after a long gap.
func (p *publishReconnectStats) sessionEnded() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastSessionEnd = time.Now()
}

// summary returns how long the current streaming period has been running
// and how many reconnects have happened during it, for the periodic log
// line an active session prints (see session.runPublishStatsSummary).
func (p *publishReconnectStats) summary() (elapsed time.Duration, reconnects int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.periodStart.IsZero() {
		return 0, 0
	}
	return time.Since(p.periodStart), p.reconnectCount
}

// getOrCreatePublishStats returns the reconnect-tracking state for path,
// creating it on first use. Plain mutex-guarded map, same pattern as
// getOrCreateDegradeState in degrade.go.
func (s *Server) getOrCreatePublishStats(path string) *publishReconnectStats {
	s.publishStatsMu.Lock()
	defer s.publishStatsMu.Unlock()
	if ps, ok := s.publishStats[path]; ok {
		return ps
	}
	ps := &publishReconnectStats{}
	s.publishStats[path] = ps
	return ps
}

// recordPublishSessionStart implements the sessionParent hook used by
// session.runPublish.
func (s *Server) recordPublishSessionStart(pathName string) publishSessionStartInfo {
	return s.getOrCreatePublishStats(pathName).sessionStarted()
}

// recordPublishSessionEnd implements the sessionParent hook used by
// session.runPublish.
func (s *Server) recordPublishSessionEnd(pathName string) {
	s.getOrCreatePublishStats(pathName).sessionEnded()
}

// publishStatsSummary implements the sessionParent hook used by
// session.runPublishStatsSummary.
func (s *Server) publishStatsSummary(pathName string) (time.Duration, int) {
	return s.getOrCreatePublishStats(pathName).summary()
}
