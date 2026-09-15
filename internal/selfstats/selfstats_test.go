package selfstats

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/logger"
)

func TestFormatBytes(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{500, "500B"},
		{2048, "2.0KB"},
		{5 * 1024 * 1024, "5.0MB"},
		{3 * 1024 * 1024 * 1024, "3.00GB"},
	}
	for _, c := range cases {
		require.Equal(t, c.want, formatBytes(c.in))
	}
}

// recordingLog captures every Log call so the test can inspect the
// rendered line without depending on a real logger.Logger sink.
type recordingLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *recordingLog) Log(_ logger.Level, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *recordingLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// TestReporterLogsPeriodically drives a Reporter with a short interval (the
// real production cadence is 90s - see Interval - far too slow for a unit
// test) and checks the rendered line carries all four requested fields.
func TestReporterLogsPeriodically(t *testing.T) {
	log := &recordingLog{}
	r := NewReporter(log, ".", 20*time.Millisecond)
	r.Start()
	defer r.Stop()

	require.Eventually(t, func() bool {
		return len(log.snapshot()) >= 2
	}, 2*time.Second, 10*time.Millisecond, "expected at least 2 [selfstats] lines")

	line := log.snapshot()[0]
	require.True(t, strings.HasPrefix(line, "[selfstats] "), line)
	require.Contains(t, line, "cpu=")
	require.Contains(t, line, "mem_alloc=")
	require.Contains(t, line, "mem_sys=")
	require.Contains(t, line, "goroutines=")
	require.Contains(t, line, "disk=")
}

func TestReporterStopEndsGoroutine(t *testing.T) {
	log := &recordingLog{}
	r := NewReporter(log, ".", 10*time.Millisecond)
	r.Start()
	require.Eventually(t, func() bool { return len(log.snapshot()) >= 1 }, time.Second, 5*time.Millisecond)

	done := make(chan struct{})
	go func() {
		r.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop did not return - run() goroutine likely leaked")
	}

	countAtStop := len(log.snapshot())
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, countAtStop, len(log.snapshot()), "no further lines should be logged after Stop")
}
