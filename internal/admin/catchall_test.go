package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProxiesToMMXWebUI(t *testing.T) {
	for _, ca := range []struct {
		name     string
		method   string
		path     string
		upgrade  string
		expected bool
	}{
		// Browser navigations that mmx would answer with a built-in page.
		{"bare name", http.MethodGet, "/login", "", true},
		{"trailing slash", http.MethodGet, "/login/", "", true},
		{"publish page", http.MethodGet, "/live/test/publish", "", true},
		{"read page", http.MethodGet, "/live/test/", "", true},
		{"favicon", http.MethodGet, "/favicon.ico", "", true},
		{"reader script", http.MethodGet, "/live/test/reader.js", "", true},

		// Media traffic, which must keep reaching mmx.
		{"whep post", http.MethodPost, "/live/test/whep", "", false},
		{"whip post", http.MethodPost, "/live/test/whip", "", false},
		{"whip options", http.MethodOptions, "/live/test/whip", "", false},
		{"whip patch in session", http.MethodPatch, "/live/test/whip/abc123", "", false},
		{"whep delete in session", http.MethodDelete, "/live/test/whep/abc123", "", false},
		// RFC draft-ietf-whip-09 wants mmx itself to answer 405 here.
		{"whep get", http.MethodGet, "/live/test/whep", "", false},

		// Control channels are WebSocket upgrades.
		{"abr control", http.MethodGet, "/ws/control", "websocket", false},
		{"degrade channel", http.MethodGet, "/live/test/ws", "WebSocket", false},
	} {
		t.Run(ca.name, func(t *testing.T) {
			req := httptest.NewRequest(ca.method, ca.path, nil)
			if ca.upgrade != "" {
				req.Header.Set("Upgrade", ca.upgrade)
			}
			require.Equal(t, ca.expected, proxiesToMMXWebUI(req))
		})
	}
}
