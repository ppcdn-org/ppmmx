package mmxcontrol

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRecordDeletionClientClaim(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/records/deletions/claim" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("limit") != "50" {
			t.Errorf("expected limit=50 in query, got %q", r.URL.RawQuery)
		}
		json.NewEncoder(w).Encode([]RecordDeletionClaim{
			{ID: 1, AppID: "app1", ObjectKey: "app1/round-1.mp4", SizeBytes: 2048},
		})
	}))
	defer server.Close()

	client := NewRecordDeletionClient(server.URL, "secret", time.Second)
	claims, err := client.Claim(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].ID != 1 || claims[0].ObjectKey != "app1/round-1.mp4" {
		t.Fatalf("unexpected claims: %+v", claims)
	}
}

func TestRecordDeletionClientClaimEmpty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]RecordDeletionClaim{})
	}))
	defer server.Close()

	client := NewRecordDeletionClient(server.URL, "secret", time.Second)
	claims, err := client.Claim(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 0 {
		t.Fatalf("expected no claims, got %+v", claims)
	}
}

func TestRecordDeletionClientComplete(t *testing.T) {
	var received RecordDeletionResult
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/records/deletions/complete" || r.Method != http.MethodPost {
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

	client := NewRecordDeletionClient(server.URL, "secret", time.Second)
	if err := client.Complete(context.Background(), 7, true, ""); err != nil {
		t.Fatal(err)
	}
	if received.ID != 7 || !received.Success {
		t.Fatalf("unexpected server-received result: %+v", received)
	}
}

func TestRecordDeletionClientCompleteFailure(t *testing.T) {
	var received RecordDeletionResult
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewRecordDeletionClient(server.URL, "secret", time.Second)
	if err := client.Complete(context.Background(), 7, false, "object not found and bucket unreachable"); err != nil {
		t.Fatal(err)
	}
	if received.ID != 7 || received.Success || received.Error == "" {
		t.Fatalf("unexpected server-received result: %+v", received)
	}
}

func TestRecordDeletionClientRequiresEndpointAndToken(t *testing.T) {
	client := NewRecordDeletionClient("", "", time.Second)
	if _, err := client.Claim(context.Background(), 10); err == nil {
		t.Fatal("expected error when endpoint/token are unset")
	}
	if err := client.Complete(context.Background(), 1, true, ""); err == nil {
		t.Fatal("expected error when endpoint/token are unset")
	}
}
