package healthlog

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluenviron/mediamtx/internal/logger"
)

// Reporter ships a batch of events to ppcenter. The concrete implementation
// (mmxcontrol.HealthLogClient) POSTs to /internal/mmx/v1/log-events.
type Reporter interface {
	ReportEvents(ctx context.Context, events []Event) error
}

// Collector buffers classified health events in a bounded queue and flushes
// them to ppcenter in batches, either once batchSize accumulates or every
// flushInterval, whichever comes first.
//
// It is safe for concurrent use and never blocks: HandleLog runs on the
// logger's hot path, so a full queue drops the event (and counts it) instead
// of stalling whatever media goroutine tried to log.
type Collector struct {
	reporter      Reporter
	queue         chan Event
	done          chan struct{}
	closeOnce     sync.Once
	closed        atomic.Bool
	dropped       atomic.Int64
	sent          atomic.Int64
	failed        atomic.Int64
	batchSize     int
	flushInterval time.Duration
}

func NewCollector(reporter Reporter, queueSize, batchSize int, flushInterval time.Duration) *Collector {
	if queueSize <= 0 || queueSize > 100000 {
		queueSize = 512
	}
	if batchSize <= 0 || batchSize > 1000 {
		batchSize = 200
	}
	if flushInterval <= 0 {
		flushInterval = 5 * time.Second
	}
	c := &Collector{
		reporter:      reporter,
		queue:         make(chan Event, queueSize),
		done:          make(chan struct{}),
		batchSize:     batchSize,
		flushInterval: flushInterval,
	}
	go c.loop()
	return c
}

// HandleLog is the logger hook. It classifies the entry and enqueues it when
// it is a recognised health event.
func (c *Collector) HandleLog(t time.Time, level logger.Level, format string, args ...any) {
	// Cheap pre-filter on the format string (no formatting/allocation) so
	// the vast majority of log lines never reach the classifier.
	if c.closed.Load() || !mightBeHealthEvent(format) {
		return
	}
	event, ok := Classify(t, level, fmt.Sprintf(format, args...))
	if !ok {
		return
	}
	c.Emit(event)
}

// Emit enqueues an already-built event. Exported so a producer that can emit
// a structured event directly does not have to go through the log line.
//
// NLOG-001 requires dropping the oldest event (not the newest) when the queue
// is full: the freshest evidence is the most useful for a recent incident,
// and the drop is counted.
func (c *Collector) Emit(event Event) bool {
	if c.closed.Load() {
		return false
	}
	select {
	case c.queue <- event:
		return true
	default:
	}
	// Queue full: evict the head to make room, then retry once.
	select {
	case <-c.queue:
		c.dropped.Add(1)
	default:
	}
	select {
	case c.queue <- event:
		return true
	default:
		c.dropped.Add(1)
		return false
	}
}

// Close stops the collector and performs one final best-effort flush.
func (c *Collector) Close() {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		close(c.done)
	})
}

// Stats returns (dropped, sent, failed) event counts for observability.
func (c *Collector) Stats() (dropped, sent, failed int64) {
	return c.dropped.Load(), c.sent.Load(), c.failed.Load()
}

func (c *Collector) loop() {
	ticker := time.NewTicker(c.flushInterval)
	defer ticker.Stop()

	batch := make([]Event, 0, c.batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		c.send(batch)
		batch = batch[:0]
	}

	for {
		select {
		case event := <-c.queue:
			batch = append(batch, event)
			if len(batch) >= c.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-c.done:
			// Drain whatever is already queued, then flush the tail so a
			// clean shutdown does not silently lose the last few events.
			for {
				select {
				case event := <-c.queue:
					batch = append(batch, event)
					if len(batch) >= c.batchSize {
						flush()
					}
					continue
				default:
				}
				break
			}
			flush()
			return
		}
	}
}

func (c *Collector) send(batch []Event) {
	if c.reporter == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := c.reporter.ReportEvents(ctx, batch); err != nil {
		// Fire-and-forget: a dropped batch is preferable to unbounded
		// buffering or blocking the reporter. The count is surfaced through
		// Stats for diagnostics.
		c.failed.Add(int64(len(batch)))
		return
	}
	c.sent.Add(int64(len(batch)))
}

func newEventID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is effectively impossible; fall back to a
		// time-based id rather than emitting an empty (non-idempotent) key.
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
