package mmxcontrol

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSegmentReporterEnqueueRejectsInvalid(t *testing.T) {
	r := NewSegmentReporter(nil, 4)
	if r.Enqueue(SegmentReport{}) {
		t.Fatal("accepted empty report")
	}
	if !r.Enqueue(SegmentReport{AppID: "a", StreamPath: "a/s", RoundID: "r", FileName: "f.m4s", DurationMS: 1000}) {
		t.Fatal("valid report rejected")
	}
}

// Backpressure is only observable while the consumer is actually busy: with
// a nil client the loop drains the queue instantly, so the original version
// of this test was timing-dependent (it passed only when the loop goroutine
// had not been scheduled yet). Point the reporter at a handler that blocks
// until released, wait until the first report is in flight, and the single
// queue slot then deterministically fills.
func TestSegmentReporterBackpressureAndClose(t *testing.T) {
	received := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case received <- struct{}{}:
		default:
		}
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewRecordingSyncClient(server.URL, "token", 5*time.Second)
	r := NewSegmentReporter(client, 1)
	if !r.Enqueue(SegmentReport{AppID: "a", StreamPath: "a/s", RoundID: "r", FileName: "f.m4s", DurationMS: 1000}) {
		t.Fatal("first report rejected")
	}
	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("consumer never attempted the first report")
	}
	if !r.Enqueue(SegmentReport{AppID: "a", StreamPath: "a/s", RoundID: "r", FileName: "g.m4s", DurationMS: 1000}) {
		t.Fatal("second report rejected while the queue had room")
	}
	if r.Enqueue(SegmentReport{AppID: "a", StreamPath: "a/s", RoundID: "r", FileName: "h.m4s", DurationMS: 1000}) {
		t.Fatal("exceeded queue capacity without backpressure")
	}
	close(release)
	r.Close()
}
