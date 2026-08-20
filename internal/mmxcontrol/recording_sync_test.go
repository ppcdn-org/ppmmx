package mmxcontrol

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRecordingSyncClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/records/tasks/active":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":1,"appId":"app","streamPath":"app/live","recordingSessionId":"recording-1","status":"active"}]`))
		case "/records/segments":
			if r.Method != http.MethodPost {
				http.Error(w, "bad method", http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := NewRecordingSyncClient(server.URL, "token", time.Second)
	tasks, err := client.ActiveTasks(context.Background())
	if err != nil || len(tasks) != 1 || tasks[0].RecordingSessionID != "recording-1" {
		t.Fatalf("tasks=%+v err=%v", tasks, err)
	}
	err = client.ReportSegment(context.Background(), SegmentMetadata{AppID: "app", StreamPath: "app/live", RoundID: "recording-1", Sequence: 0, FileName: "seg.m4s", DurationMS: 1000})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRecordingSyncClientRejectsInvalidTask(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"appId":"app","streamPath":"app/live","status":"active"}]`))
	}))
	defer server.Close()
	if _, err := NewRecordingSyncClient(server.URL, "token", time.Second).ActiveTasks(context.Background()); err == nil {
		t.Fatal("accepted task without recording session")
	}
}
