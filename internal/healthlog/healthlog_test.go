package healthlog

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/logger"
)

func TestClassifyDegradeTransition(t *testing.T) {
	event, ok := Classify(time.Now(), logger.Info, "[degrade] path=appA/table1 -> layers=2 bitrate=80%")
	require.True(t, ok)
	require.Equal(t, CategoryDegradeTransition, event.Category)
	require.Equal(t, "step_down", event.Code)
	require.Equal(t, "appA/table1", event.StreamPath)
	require.Equal(t, 2, event.Fields["layers"])
	require.Equal(t, 80, event.Fields["bitratePercent"])
	require.NotEmpty(t, event.EventID)
}

func TestClassifyDegradeTerminalAlert(t *testing.T) {
	event, ok := Classify(time.Now(), logger.Warn,
		"[degrade] path=appA/table1 layers=1 bitrate=80% still non-compliant, giving up")
	require.True(t, ok)
	require.Equal(t, CategoryDegradeAlert, event.Category)
	require.Equal(t, "terminal", event.Code)
}

func TestClassifyPublishReconnect(t *testing.T) {
	event, ok := Classify(time.Now(), logger.Warn,
		"[publish-stats] path=appA/table1 reconnected (likely RTP loss/network drop) - this is reconnect #2 since streaming started at 2026-09-16T00:00:00Z")
	require.True(t, ok)
	require.Equal(t, CategoryPublishReconnect, event.Category)
	require.Equal(t, "appA/table1", event.StreamPath)
}

func TestClassifyErrorDumperAggregate(t *testing.T) {
	event, ok := Classify(time.Now(), logger.Warn, "12 decode errors, last was: unexpected EOF")
	require.True(t, ok)
	require.Equal(t, CategoryNodeErrorDump, event.Category)
	require.Equal(t, "media_decode", event.Code)

	event, ok = Classify(time.Now(), logger.Warn, "5 processing errors (0.10% of 5000 RTP packets), last was: bad")
	require.True(t, ok)
	require.Equal(t, CategoryNodeErrorDump, event.Category)
}

func TestClassifyIgnoresUnrelatedLines(t *testing.T) {
	for _, line := range []string{
		"[selfstats] cpu=1% mem_alloc=2GB",
		"[publish-stats] path=appA/table1 streaming for 3m0s, 0 reconnect(s) so far",
		"random application message",
	} {
		_, ok := Classify(time.Now(), logger.Info, line)
		require.False(t, ok, "line should not be classified: %s", line)
	}
}

func TestMightBeHealthEventPreFilter(t *testing.T) {
	require.True(t, mightBeHealthEvent("[degrade] path=%s -> layers=%d bitrate=%d%%"))
	require.True(t, mightBeHealthEvent("[publish-stats] path=%s reconnected"))
	require.False(t, mightBeHealthEvent("[selfstats] cpu=%s mem_alloc=%s"))
	require.False(t, mightBeHealthEvent("recording stopped"))
}

func TestCollectorFlushesBatchOnSize(t *testing.T) {
	reporter := &recordingReporter{}
	c := NewCollector(reporter, 16, 2, time.Hour)
	defer c.Close()

	c.Emit(Event{Category: CategoryPublishReconnect})
	c.Emit(Event{Category: CategoryDegradeTransition})

	require.Eventually(t, func() bool {
		return len(reporter.batches()) == 1
	}, 2*time.Second, 10*time.Millisecond)
	require.Len(t, reporter.batches()[0], 2)

	dropped, sent, failed := c.Stats()
	require.Zero(t, dropped)
	require.EqualValues(t, 2, sent)
	require.Zero(t, failed)
}

func TestCollectorDropsWhenFullAndNeverBlocks(t *testing.T) {
	block := make(chan struct{})
	reporter := &blockingReporter{gate: block}
	// batchSize=1 means the loop takes one event and immediately blocks in
	// the reporter; the queue then fills and further emits are dropped.
	c := NewCollector(reporter, 1, 1, time.Hour)
	defer func() {
		close(block)
		c.Close()
	}()

	c.Emit(Event{Category: CategoryPublishReconnect}) // consumed, reporter blocks
	c.Emit(Event{Category: CategoryPublishReconnect}) // sits in queue
	// Give the loop a moment to take the first event.
	require.Eventually(t, func() bool { return reporter.callsCount() >= 1 }, time.Second, 5*time.Millisecond)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			c.Emit(Event{Category: CategoryPublishReconnect})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Emit blocked when the queue was full")
	}
	dropped, _, _ := c.Stats()
	require.Greater(t, dropped, int64(0))
}

type recordingReporter struct {
	mu      sync.Mutex
	records [][]Event
}

func (r *recordingReporter) ReportEvents(_ context.Context, events []Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, append([]Event(nil), events...))
	return nil
}

func (r *recordingReporter) batches() [][]Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.records
}

type blockingReporter struct {
	gate      chan struct{}
	mu        sync.Mutex
	callCount int
}

func (r *blockingReporter) ReportEvents(_ context.Context, _ []Event) error {
	r.mu.Lock()
	r.callCount++
	r.mu.Unlock()
	<-r.gate
	return nil
}

func (r *blockingReporter) callsCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.callCount
}
