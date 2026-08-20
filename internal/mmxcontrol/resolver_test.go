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

func TestOriginResolverAndCache(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		require.Equal(t, "Bearer node-secret", r.Header.Get("Authorization"))
		var request map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		require.Equal(t, "app/live", request["streamPath"])
		json.NewEncoder(w).Encode(map[string]any{
			"whepUrl":     "https://origin.example/app/live/whep",
			"bearerToken": "u123:secret",
			"expiresAt":   time.Now().Add(time.Minute),
		})
	}))

	resolver := NewOriginResolver(server.URL, "node-secret", "Sydney", 7, time.Second)
	resolved, err := resolver.ResolveSource(context.Background(), "app/live", "whep://placeholder")
	require.NoError(t, err)
	require.Equal(t, "wheps://origin.example/app/live/whep", resolved.Source)
	require.Equal(t, "u123:secret", resolved.WHEPBearerToken)

	server.Close()
	cached, err := resolver.ResolveSource(context.Background(), "app/live", "whep://placeholder")
	require.NoError(t, err)
	require.Equal(t, resolved, cached)
	require.Equal(t, 1, requests)
}

func TestOriginResolverRejectsInvalidResponseWithoutCache(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"whepUrl": "relative", "bearerToken": "secret", "expiresAt": time.Now().Add(time.Minute),
		})
	}))
	defer server.Close()

	resolver := NewOriginResolver(server.URL, "node-secret", "Sydney", 7, time.Second)
	_, err := resolver.ResolveSource(context.Background(), "app/live", "whep://placeholder")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "node-secret")
}
