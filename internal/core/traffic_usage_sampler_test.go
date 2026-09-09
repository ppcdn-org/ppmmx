package core

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/mmxcontrol"
)

type fakeTrafficUsageServer struct {
	mu    sync.Mutex
	items []defs.APIWebRTCSession
}

func (f *fakeTrafficUsageServer) APISessionsList() (*defs.APIWebRTCSessionList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &defs.APIWebRTCSessionList{ItemCount: len(f.items), Items: append([]defs.APIWebRTCSession(nil), f.items...)}, nil
}

func (f *fakeTrafficUsageServer) setItems(items []defs.APIWebRTCSession) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items = items
}

type fakeTrafficUsageReporter struct {
	mu      sync.Mutex
	reports []mmxcontrol.TrafficUsageReport
}

func (f *fakeTrafficUsageReporter) Report(_ context.Context, report mmxcontrol.TrafficUsageReport) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports = append(f.reports, report)
	return nil
}

func (f *fakeTrafficUsageReporter) snapshot() []mmxcontrol.TrafficUsageReport {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mmxcontrol.TrafficUsageReport(nil), f.reports...)
}

type nopLogger struct{}

func (nopLogger) Log(logger.Level, string, ...any) {}

func TestTrafficUsageSamplerReportsFirstSampleAsFullDelta(t *testing.T) {
	server := &fakeTrafficUsageServer{}
	reporter := &fakeTrafficUsageReporter{}
	s := &TrafficUsageSampler{Server: server, Reporter: reporter, Parent: nopLogger{}}
	s.Initialize()
	defer s.Close()

	server.setItems([]defs.APIWebRTCSession{
		{ID: uuid.New(), State: defs.APIWebRTCSessionStateRead, Path: "app1/stream", OutboundBytes: 1000},
	})
	s.sampleOnce()

	require.Eventually(t, func() bool { return len(reporter.snapshot()) == 1 }, time.Second, 10*time.Millisecond)
	got := reporter.snapshot()[0]
	require.Equal(t, "app1", got.AppID)
	require.Equal(t, int64(1000), got.DownstreamDeltaBytes)
}

func TestTrafficUsageSamplerReportsIncrementalDeltaOnly(t *testing.T) {
	server := &fakeTrafficUsageServer{}
	reporter := &fakeTrafficUsageReporter{}
	s := &TrafficUsageSampler{Server: server, Reporter: reporter, Parent: nopLogger{}}
	s.Initialize()
	defer s.Close()

	sessionID := uuid.New()
	server.setItems([]defs.APIWebRTCSession{
		{ID: sessionID, State: defs.APIWebRTCSessionStateRead, Path: "app1/stream", OutboundBytes: 1000},
	})
	s.sampleOnce()
	require.Eventually(t, func() bool { return len(reporter.snapshot()) == 1 }, time.Second, 10*time.Millisecond)

	server.setItems([]defs.APIWebRTCSession{
		{ID: sessionID, State: defs.APIWebRTCSessionStateRead, Path: "app1/stream", OutboundBytes: 2500},
	})
	s.sampleOnce()
	require.Eventually(t, func() bool { return len(reporter.snapshot()) == 2 }, time.Second, 10*time.Millisecond)

	got := reporter.snapshot()[1]
	require.Equal(t, int64(1500), got.DownstreamDeltaBytes)
}

func TestTrafficUsageSamplerSkipsPublishSessions(t *testing.T) {
	server := &fakeTrafficUsageServer{}
	reporter := &fakeTrafficUsageReporter{}
	s := &TrafficUsageSampler{Server: server, Reporter: reporter, Parent: nopLogger{}}
	s.Initialize()
	defer s.Close()

	server.setItems([]defs.APIWebRTCSession{
		{ID: uuid.New(), State: defs.APIWebRTCSessionStatePublish, Path: "app1/stream", OutboundBytes: 999999},
	})
	s.sampleOnce()

	time.Sleep(100 * time.Millisecond)
	require.Empty(t, reporter.snapshot())
}

func TestTrafficUsageSamplerSkipsZeroAndStaleDeltas(t *testing.T) {
	server := &fakeTrafficUsageServer{}
	reporter := &fakeTrafficUsageReporter{}
	s := &TrafficUsageSampler{Server: server, Reporter: reporter, Parent: nopLogger{}}
	s.Initialize()
	defer s.Close()

	sessionID := uuid.New()
	server.setItems([]defs.APIWebRTCSession{
		{ID: sessionID, State: defs.APIWebRTCSessionStateRead, Path: "app1/stream", OutboundBytes: 1000},
	})
	s.sampleOnce()
	require.Eventually(t, func() bool { return len(reporter.snapshot()) == 1 }, time.Second, 10*time.Millisecond)

	// No growth this round: must not report a zero/negative delta.
	server.setItems([]defs.APIWebRTCSession{
		{ID: sessionID, State: defs.APIWebRTCSessionStateRead, Path: "app1/stream", OutboundBytes: 1000},
	})
	s.sampleOnce()
	time.Sleep(100 * time.Millisecond)
	require.Len(t, reporter.snapshot(), 1, "flat OutboundBytes must not produce a second report")
}

func TestTrafficUsageSamplerDropsSessionsWithoutParsablePath(t *testing.T) {
	server := &fakeTrafficUsageServer{}
	reporter := &fakeTrafficUsageReporter{}
	s := &TrafficUsageSampler{Server: server, Reporter: reporter, Parent: nopLogger{}}
	s.Initialize()
	defer s.Close()

	server.setItems([]defs.APIWebRTCSession{
		{ID: uuid.New(), State: defs.APIWebRTCSessionStateRead, Path: "", OutboundBytes: 1000},
	})
	s.sampleOnce()

	time.Sleep(100 * time.Millisecond)
	require.Empty(t, reporter.snapshot())
}

func TestTrafficUsageSamplerPrunesClosedSessions(t *testing.T) {
	server := &fakeTrafficUsageServer{}
	reporter := &fakeTrafficUsageReporter{}
	s := &TrafficUsageSampler{Server: server, Reporter: reporter, Parent: nopLogger{}}
	s.Initialize()
	defer s.Close()

	sessionID := uuid.New()
	server.setItems([]defs.APIWebRTCSession{
		{ID: sessionID, State: defs.APIWebRTCSessionStateRead, Path: "app1/stream", OutboundBytes: 1000},
	})
	s.sampleOnce()
	require.Eventually(t, func() bool { return len(reporter.snapshot()) == 1 }, time.Second, 10*time.Millisecond)

	s.mu.Lock()
	_, tracked := s.lastSeen[sessionID.String()]
	s.mu.Unlock()
	require.True(t, tracked)

	// Session closed: gone from the next list.
	server.setItems(nil)
	s.sampleOnce()

	s.mu.Lock()
	_, stillTracked := s.lastSeen[sessionID.String()]
	s.mu.Unlock()
	require.False(t, stillTracked, "closed session must be pruned from lastSeen")
}

func TestAppIDFromPath(t *testing.T) {
	cases := map[string]string{
		"app1/stream":          "app1",
		"app1/stream/h264":     "app1",
		"app1/stream/hevc":     "app1",
		"":                     "",
		"  app1/stream  ":      "app1",
		"noAppIdSeparatorHere": "noAppIdSeparatorHere",
	}
	for input, want := range cases {
		require.Equal(t, want, appIDFromPath(input), "input=%q", input)
	}
}
