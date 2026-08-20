package mmxcontrol

import (
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

func TestSegmentReporterBackpressureAndClose(t *testing.T) {
	r := NewSegmentReporter(nil, 1)
	_ = r.Enqueue(SegmentReport{AppID: "a", StreamPath: "a/s", RoundID: "r", FileName: "f.m4s", DurationMS: 1000})
	if r.Enqueue(SegmentReport{AppID: "a", StreamPath: "a/s", RoundID: "r", FileName: "g.m4s", DurationMS: 1000}) {
		t.Fatal("exceeded queue capacity without backpressure")
	}
	r.Close()
	time.Sleep(5 * time.Millisecond)
}
