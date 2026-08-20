package mmxcontrol

import (
	"context"
	"sync"
	"sync/atomic"
)

type SegmentReport struct {
	AppID      string
	StreamPath string
	RoundID    string
	Sequence   int64
	FileName   string
	LocalPath  string
	DurationMS int64
	SizeBytes  int64
}

type SegmentReporter struct {
	client    *RecordingSyncClient
	queue     chan SegmentReport
	done      chan struct{}
	closeOnce sync.Once
	closed    atomic.Bool
	seq       atomic.Int64
}

func NewSegmentReporter(client *RecordingSyncClient, queueSize int) *SegmentReporter {
	if queueSize <= 0 || queueSize > 10000 {
		queueSize = 256
	}
	r := &SegmentReporter{
		client: client,
		queue:  make(chan SegmentReport, queueSize),
		done:   make(chan struct{}),
	}
	go r.loop()
	return r
}

func (r *SegmentReporter) Enqueue(report SegmentReport) bool {
	if r.closed.Load() {
		return false
	}
	if report.AppID == "" || report.StreamPath == "" || report.RoundID == "" || report.FileName == "" || report.DurationMS <= 0 || report.SizeBytes < 0 {
		return false
	}
	report.Sequence = r.seq.Add(1)
	select {
	case r.queue <- report:
		return true
	default:
		r.seq.Add(-1)
		return false
	}
}

func (r *SegmentReporter) Close() {
	r.closeOnce.Do(func() {
		r.closed.Store(true)
		close(r.done)
	})
}

func (r *SegmentReporter) loop() {
	ctx := context.Background()
	for {
		select {
		case report := <-r.queue:
			if r.client == nil {
				continue
			}
			meta := SegmentMetadata{
				AppID: report.AppID, StreamPath: report.StreamPath, RoundID: report.RoundID,
				Sequence: report.Sequence, FileName: report.FileName, LocalPath: report.LocalPath,
				DurationMS: report.DurationMS, SizeBytes: report.SizeBytes,
			}
			_ = r.client.ReportSegment(ctx, meta)
		case <-r.done:
			return
		}
	}
}
