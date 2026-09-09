package mmxcontrol

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTrafficUsageClientReport(t *testing.T) {
	var received TrafficUsageReport
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/traffic/usage" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewTrafficUsageClient(server.URL, "secret", time.Second)
	err := client.Report(context.Background(), TrafficUsageReport{
		AppID: "app1", UsageDate: "2026-01-15", DownstreamDeltaBytes: 12345,
	})
	if err != nil {
		t.Fatal(err)
	}
	if received.AppID != "app1" || received.UsageDate != "2026-01-15" || received.DownstreamDeltaBytes != 12345 {
		t.Fatalf("unexpected server-received report: %+v", received)
	}
}

func TestTrafficUsageClientSkipsZeroDelta(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewTrafficUsageClient(server.URL, "secret", time.Second)
	if err := client.Report(context.Background(), TrafficUsageReport{AppID: "app1", UsageDate: "2026-01-15", DownstreamDeltaBytes: 0}); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("a zero-byte delta must not make an HTTP call")
	}
}

func TestTrafficUsageClientRejectsInvalidReport(t *testing.T) {
	client := NewTrafficUsageClient("http://example.invalid", "secret", time.Second)

	cases := []TrafficUsageReport{
		{AppID: "", UsageDate: "2026-01-15", DownstreamDeltaBytes: 100},
		{AppID: "app1", UsageDate: "", DownstreamDeltaBytes: 100},
		{AppID: "app1", UsageDate: "2026-01-15", DownstreamDeltaBytes: -1},
	}
	for _, tc := range cases {
		if err := client.Report(context.Background(), tc); err == nil {
			t.Fatalf("expected error for %+v", tc)
		}
	}
}

func TestTrafficUsageClientPropagatesServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	client := NewTrafficUsageClient(server.URL, "secret", time.Second)
	err := client.Report(context.Background(), TrafficUsageReport{AppID: "app1", UsageDate: "2026-01-15", DownstreamDeltaBytes: 100})
	if err == nil {
		t.Fatal("expected error on non-200 response")
	}
}
