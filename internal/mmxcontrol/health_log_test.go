package mmxcontrol

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/healthlog"
)

func TestHealthLogClientReportEvents(t *testing.T) {
	var gotAuth, gotEncoding string
	var decoded healthLogReportRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotEncoding = r.Header.Get("Content-Encoding")
		require.Equal(t, "/log-events", r.URL.Path)

		reader := io.Reader(r.Body)
		if gotEncoding == "gzip" {
			gz, err := gzip.NewReader(r.Body)
			require.NoError(t, err)
			defer gz.Close()
			reader = gz
		}
		body, err := io.ReadAll(reader)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &decoded))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewHealthLogClient(server.URL, "secret-token", 0)
	err := client.ReportEvents(context.Background(), []healthlog.Event{
		{EventID: "e1", Category: healthlog.CategoryPublishReconnect, Message: "reconnected"},
	})
	require.NoError(t, err)
	require.Equal(t, "Bearer secret-token", gotAuth)
	require.Equal(t, "gzip", gotEncoding)
	require.Len(t, decoded.Events, 1)
	require.Equal(t, "e1", decoded.Events[0].EventID)
}

func TestHealthLogClientReturnsErrorOnBadStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := NewHealthLogClient(server.URL, "secret-token", 0)
	err := client.ReportEvents(context.Background(), []healthlog.Event{{EventID: "e1"}})
	require.Error(t, err)
	require.Contains(t, err.Error(), fmt.Sprintf("status %d", http.StatusInternalServerError))
}

func TestHealthLogClientNoopOnEmptyBatch(t *testing.T) {
	client := NewHealthLogClient("", "", 0)
	require.NoError(t, client.ReportEvents(context.Background(), nil))
}
