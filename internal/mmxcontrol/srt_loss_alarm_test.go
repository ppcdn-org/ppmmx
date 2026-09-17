package mmxcontrol

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSRTLossAlarmClientReport(t *testing.T) {
	var received SRTLossAlarmReport
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/alarms/srt-loss" || r.Method != http.MethodPost {
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

	client := NewSRTLossAlarmClient(server.URL, "secret", time.Second)
	err := client.Report(context.Background(), SRTLossAlarmReport{
		PathName: "app1/table-view", UnrecoveredPct: 14.2, BitrateBps: 5_300_000, SustainedSec: 120,
	})
	if err != nil {
		t.Fatal(err)
	}
	if received.PathName != "app1/table-view" || received.UnrecoveredPct != 14.2 || received.SustainedSec != 120 {
		t.Fatalf("unexpected server-received report: %+v", received)
	}
}

// A zero UnrecoveredPct is the legitimate "resolved" report and must still
// reach the server, unlike TrafficUsageClient's zero-delta skip.
func TestSRTLossAlarmClientSendsZeroLossResolve(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewSRTLossAlarmClient(server.URL, "secret", time.Second)
	if err := client.Report(context.Background(), SRTLossAlarmReport{PathName: "app1/table-view", UnrecoveredPct: 0}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("a zero-loss resolve report must still make an HTTP call")
	}
}

func TestSRTLossAlarmClientRejectsInvalidReport(t *testing.T) {
	client := NewSRTLossAlarmClient("http://example.invalid", "secret", time.Second)

	cases := []SRTLossAlarmReport{
		{PathName: "", UnrecoveredPct: 10},
		{PathName: "app1/table-view", UnrecoveredPct: -1},
	}
	for _, tc := range cases {
		if err := client.Report(context.Background(), tc); err == nil {
			t.Fatalf("expected error for %+v", tc)
		}
	}
}

func TestSRTLossAlarmClientPropagatesServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	client := NewSRTLossAlarmClient(server.URL, "secret", time.Second)
	err := client.Report(context.Background(), SRTLossAlarmReport{PathName: "app1/table-view", UnrecoveredPct: 15})
	if err == nil {
		t.Fatal("expected error on non-200 response")
	}
}
