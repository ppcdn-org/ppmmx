package mmxcontrol

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAppSyncClientFetch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/app-credentials/sync", r.URL.Path)
		require.Equal(t, "Bearer node-secret", r.Header.Get("Authorization"))
		json.NewEncoder(w).Encode(map[string]any{
			"apps": []map[string]string{
				{"appId": "app1", "appSecret": "secret1"},
				{"appId": "app2", "appSecret": "secret2"},
			},
		})
	}))
	defer server.Close()

	client := NewAppSyncClient(server.URL, "node-secret", time.Second)
	creds, err := client.Fetch(context.Background())
	require.NoError(t, err)
	require.Equal(t, []AppCredential{
		{AppID: "app1", AppSecret: "secret1"},
		{AppID: "app2", AppSecret: "secret2"},
	}, creds)
}

func TestAppSyncClientFetchEmptyList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"apps": []map[string]string{}})
	}))
	defer server.Close()

	client := NewAppSyncClient(server.URL, "node-secret", time.Second)
	creds, err := client.Fetch(context.Background())
	require.NoError(t, err)
	require.Empty(t, creds)
}

func TestAppSyncClientRejectsNonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	client := NewAppSyncClient(server.URL, "node-secret", time.Second)
	_, err := client.Fetch(context.Background())
	require.Error(t, err)
}

func TestAppSyncClientRequiresBaseURLAndToken(t *testing.T) {
	client := NewAppSyncClient("", "", time.Second)
	_, err := client.Fetch(context.Background())
	require.Error(t, err)
}
