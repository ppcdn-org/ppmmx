package webrtc

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPublishStatsFirstSessionIsNotAReconnect(t *testing.T) {
	var p publishReconnectStats
	info := p.sessionStarted()
	require.False(t, info.isReconnect)
	require.Equal(t, 0, info.reconnectCount)
	require.False(t, info.streamingSince.IsZero())
}

func TestPublishStatsQuickReconnectIncrementsCount(t *testing.T) {
	var p publishReconnectStats
	first := p.sessionStarted()
	p.sessionEnded()

	second := p.sessionStarted()
	require.True(t, second.isReconnect)
	require.Equal(t, 1, second.reconnectCount)
	require.Equal(t, first.streamingSince, second.streamingSince, "streaming period must not reset on a quick reconnect")

	p.sessionEnded()
	third := p.sessionStarted()
	require.True(t, third.isReconnect)
	require.Equal(t, 2, third.reconnectCount)
}

func TestPublishStatsLongGapStartsFreshPeriod(t *testing.T) {
	var p publishReconnectStats
	p.sessionStarted()
	p.mu.Lock()
	p.lastSessionEnd = time.Now().Add(-2 * publishStatsStaleGap) // simulate a long-ago disconnect
	p.mu.Unlock()

	info := p.sessionStarted()
	require.False(t, info.isReconnect, "a gap longer than publishStatsStaleGap must start a fresh streaming period")
	require.Equal(t, 0, info.reconnectCount)
}

func TestPublishStatsSummaryBeforeAnySession(t *testing.T) {
	var p publishReconnectStats
	elapsed, reconnects := p.summary()
	require.Equal(t, time.Duration(0), elapsed)
	require.Equal(t, 0, reconnects)
}

func TestPublishStatsSummaryReflectsReconnectCount(t *testing.T) {
	var p publishReconnectStats
	p.sessionStarted()
	p.sessionEnded()
	p.sessionStarted()

	elapsed, reconnects := p.summary()
	require.GreaterOrEqual(t, elapsed, time.Duration(0))
	require.Equal(t, 1, reconnects)
}

func TestServerGetOrCreatePublishStatsReturnsSameInstance(t *testing.T) {
	s := &Server{}
	s.publishStats = make(map[string]*publishReconnectStats)

	a := s.getOrCreatePublishStats("live/table1-fwv")
	b := s.getOrCreatePublishStats("live/table1-fwv")
	require.Same(t, a, b)

	c := s.getOrCreatePublishStats("live/table1-fwh")
	require.NotSame(t, a, c)
}
