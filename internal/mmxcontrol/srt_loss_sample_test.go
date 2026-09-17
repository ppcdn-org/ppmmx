package mmxcontrol

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSRTLossSampleClientReport(t *testing.T) {
	var received SRTLossSampleReport
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/srt-loss-samples" || r.Method != http.MethodPost {
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

	client := NewSRTLossSampleClient(server.URL, "secret", time.Second)
	err := client.Report(context.Background(), SRTLossSampleReport{
		PathName:         "app1/table-view/h264",
		RecoverablePct:   9.8,
		UnrecoverablePct: 0.13,
		BitrateBps:       4_500_000,
		MinuteUTC:        "2026-09-17 16:42",
	})
	if err != nil {
		t.Fatal(err)
	}
	if received.PathName != "app1/table-view/h264" || received.RecoverablePct != 9.8 ||
		received.UnrecoverablePct != 0.13 || received.MinuteUTC != "2026-09-17 16:42" {
		t.Fatalf("unexpected server-received report: %+v", received)
	}
}

// A zero-loss minute is a real data point for a trend, unlike
// TrafficUsageClient's zero-delta skip - it must still be sent.
func TestSRTLossSampleClientSendsZeroLoss(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewSRTLossSampleClient(server.URL, "secret", time.Second)
	report := SRTLossSampleReport{PathName: "app1/s", MinuteUTC: "2026-09-17 16:42"}
	if err := client.Report(context.Background(), report); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("a zero-loss sample must still make an HTTP call")
	}
}

func TestSRTLossSampleClientRejectsInvalidReport(t *testing.T) {
	client := NewSRTLossSampleClient("http://example.invalid", "secret", time.Second)

	cases := []SRTLossSampleReport{
		{PathName: "", MinuteUTC: "2026-09-17 16:42"},
		{PathName: "app1/s", RecoverablePct: -1},
		{PathName: "app1/s", UnrecoverablePct: -0.5},
		{PathName: "app1/s", BitrateBps: -1},
	}
	for _, tc := range cases {
		if err := client.Report(context.Background(), tc); err == nil {
			t.Fatalf("expected error for %+v", tc)
		}
	}
}

func TestSRTLossSampleClientRequiresEndpointAndToken(t *testing.T) {
	client := NewSRTLossSampleClient("", "", time.Second)
	if err := client.Report(context.Background(), SRTLossSampleReport{PathName: "app1/s"}); err == nil {
		t.Fatal("expected error when endpoint/token are unset")
	}
}

func TestSRTLossSampleClientPropagatesServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	client := NewSRTLossSampleClient(server.URL, "secret", time.Second)
	if err := client.Report(context.Background(), SRTLossSampleReport{PathName: "app1/s"}); err == nil {
		t.Fatal("expected error on non-200 response")
	}
}
