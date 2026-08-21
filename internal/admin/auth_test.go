package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// fakeMMXAPI starts an httptest server that answers /v3/paths/get/<name>
// and /v3/config/paths/get/<confName> the way mmx's own Control API would,
// for testing playUriHandler's simulcastLayerCount without needing a real
// mmx instance. layers=0 simulates a path that isn't live (mmxGetAPI
// returns 404, matching a real "path not found" response).
func fakeMMXAPI(t *testing.T, layers int, forwardTencent bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v3/paths/get/live/test", func(w http.ResponseWriter, r *http.Request) {
		if layers == 0 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		tracks := make([]map[string]string, layers)
		for i := range tracks {
			tracks[i] = map[string]string{"codec": "H264"}
		}
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"confName": "all",
			"tracks2":  tracks,
		})
	})
	mux.HandleFunc("/v3/config/paths/get/all", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"forwardTencent": forwardTencent}) //nolint:errcheck
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestPlayURIUsesRequestHostByDefault(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "admin.db3"))
	require.NoError(t, err)
	defer store.Close()
	mmxAPI := fakeMMXAPI(t, 3, true)
	// MMXBackend is mmx's internal loopback address, not something a remote
	// client could ever reach; the default play domain must come from the
	// Host the client actually used to reach this admin server instead -
	// but always paired with webrtcAddress's own port (8890 here), not the
	// port the admin request itself came in on.
	s := &Server{MMXBackend: "http://127.0.0.1:8890", MMXAPI: mmxAPI.URL, Store: store, TXSecretKeyBack: "secret"}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/playUri", s.playUriHandler)
	req := httptest.NewRequest(http.MethodGet, "/api/playUri?app=live&stream=test", nil)
	req.Host = "mmx.example.com:8080"
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)
	require.Equal(t, http.StatusOK, res.Code)
	var body struct {
		Data struct {
			Main   map[string]string `json:"main"`
			Backup map[string]string `json:"backup"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &body))

	// 3 live H264 layers -> 3 quality keys, all sharing the same URL
	// (main negotiates simulcast itself; port comes from webrtcAddress
	// (8890), not the admin request's own port (8080), and no
	// txTime/txSecret since this endpoint doesn't check for one).
	require.Len(t, body.Data.Main, 3)
	for _, key := range []string{"q0", "q1", "q2"} {
		require.Equal(t, "http://mmx.example.com:8890/live/test/whep", body.Data.Main[key])
	}

	// backup: one signed URL per layer, each a distinct Tencent stream key.
	require.Len(t, body.Data.Backup, 3)
	for i, suffix := range []string{"", "_standard", "_economic"} {
		key := fmt.Sprintf("q%d", i)
		playURL, err := url.Parse(body.Data.Backup[key])
		require.NoError(t, err)
		require.Equal(t, "/live/test"+suffix, playURL.Path)
		expires, err := strconv.ParseInt(playURL.Query().Get("txTime"), 16, 64)
		require.NoError(t, err)
		require.WithinDuration(t, time.Now().Add(defaultPlaySignatureTTL), time.Unix(expires, 0), 2*time.Second)
	}
}

func TestPlayURIMainDomainOverrideIgnoresStoredPort(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "admin.db3"))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.SetPlayerConfig("studio.example.com:9999", "play.example.com"))
	mmxAPI := fakeMMXAPI(t, 1, false)
	s := &Server{MMXBackend: "http://127.0.0.1:8890", MMXAPI: mmxAPI.URL, Store: store, TXSecretKeyBack: "secret"}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/playUri", s.playUriHandler)
	req := httptest.NewRequest(http.MethodGet, "/api/playUri?app=live&stream=test", nil)
	req.Host = "mmx.example.com:8080"
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)
	require.Equal(t, http.StatusOK, res.Code)
	var body struct {
		Data struct {
			Main map[string]string `json:"main"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &body))
	// Host comes from the stored override, but its own port (9999) is
	// still discarded in favor of webrtcAddress's port (8890).
	require.Equal(t, "http://studio.example.com:8890/live/test/whep", body.Data.Main["q0"])
}

// TestPlayURIOmitsBackupWhenForwardTencentDisabled verifies that "backup"
// is absent from the response entirely (not an empty object) when the
// path's matched configuration doesn't have forwardTencent enabled -
// there's nothing playable on the Tencent side to offer a URL for.
func TestPlayURIOmitsBackupWhenForwardTencentDisabled(t *testing.T) {
	mmxAPI := fakeMMXAPI(t, 2, false)
	s := &Server{MMXBackend: "http://127.0.0.1:8890", MMXAPI: mmxAPI.URL, TXSecretKeyBack: "secret"}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/playUri", s.playUriHandler)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/playUri?app=live&stream=test", nil))
	require.Equal(t, http.StatusOK, res.Code)

	var body struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &body))
	_, hasBackup := body.Data["backup"]
	require.False(t, hasBackup, "backup key must be absent, not an empty object, when forwardTencent is off")

	var main map[string]string
	require.NoError(t, json.Unmarshal(body.Data["main"], &main))
	require.Len(t, main, 2)
}

// TestPlayURIReturnsEmptyWhenPathNotLive verifies that a table with no
// active publisher (or that doesn't exist at all) gets an empty "main" and
// no "backup", rather than an error - the caller may poll this before a
// stream goes live.
func TestPlayURIReturnsEmptyWhenPathNotLive(t *testing.T) {
	mmxAPI := fakeMMXAPI(t, 0, true)
	s := &Server{MMXBackend: "http://127.0.0.1:8890", MMXAPI: mmxAPI.URL, TXSecretKeyBack: "secret"}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/playUri", s.playUriHandler)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/playUri?app=live&stream=test", nil))
	require.Equal(t, http.StatusOK, res.Code)

	var body struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &body))
	_, hasBackup := body.Data["backup"]
	require.False(t, hasBackup)

	var main map[string]string
	require.NoError(t, json.Unmarshal(body.Data["main"], &main))
	require.Empty(t, main)
}

// TestV3HandlerReceivesFullPathWithV3Prefix guards against a real bug: the
// admin router mounted /v3/*path and rewrote the request's URL.Path to
// c.Param("path") (which strips the "/v3" prefix, e.g. "/v3/info" ->
// "/info") before forwarding to v3Handler - but v3Handler's own routes
// (see API.RegisterV3Routes) are registered under a "/v3" group, so every
// request 404'd once mounted this way. v3Handler must receive the request
// with its path untouched.
func TestV3HandlerReceivesFullPathWithV3Prefix(t *testing.T) {
	var receivedPath string
	v3Handler := gin.New()
	v3Handler.GET("/v3/info", func(c *gin.Context) {
		receivedPath = c.Request.URL.Path
		c.JSON(http.StatusOK, gin.H{"version": "v1.19.1"})
	})

	s := &Server{}
	s.SetV3Handler(v3Handler)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Any("/v3/*path", func(c *gin.Context) {
		s.v3Handler.ServeHTTP(c.Writer, c.Request)
	})

	res := httptest.NewRecorder()
	router.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/v3/info", nil))
	require.Equal(t, http.StatusOK, res.Code)
	require.Equal(t, "/v3/info", receivedPath, "v3Handler must see the request's original path, including the /v3 prefix")
}

// TestTxURLQualityLayers verifies that a bare stream name (no quality
// suffix) expands into the 4-layer quality ladder using the real Tencent
// stream key suffixes (see internal/forward/layers.go's layerSuffixes),
// not the legacy "_q0"/"_q1"/"_q2" scheme.
func TestTxURLQualityLayers(t *testing.T) {
	s := &Server{Store: nil, TXSecretKeyBack: "secret"}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/play/txUrl", s.txUrlHandler)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/play/txUrl?stream=table-view", nil))
	require.Equal(t, http.StatusOK, res.Code)
	var body struct {
		Data struct {
			Streams []struct {
				WebRTC string `json:"webrtc"`
			} `json:"streams"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &body))
	require.Len(t, body.Data.Streams, 4)
	require.Contains(t, body.Data.Streams[0].WebRTC, "/live/table-view?")
	require.Contains(t, body.Data.Streams[1].WebRTC, "/live/table-view_standard?")
	require.Contains(t, body.Data.Streams[2].WebRTC, "/live/table-view_economic?")
	require.Contains(t, body.Data.Streams[3].WebRTC, "/live/table-view_lite?")
}

// TestTxURLQualityStreamPassesThrough verifies that a stream name already
// carrying a real Tencent quality suffix is treated as a complete key and
// returned as a single URL, not re-expanded into the ladder.
func TestTxURLQualityStreamPassesThrough(t *testing.T) {
	s := &Server{Store: nil, TXSecretKeyBack: "secret"}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/play/txUrl", s.txUrlHandler)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/play/txUrl?stream=table-view_standard", nil))
	require.Equal(t, http.StatusOK, res.Code)
	var body struct {
		Data struct {
			WebRTC string `json:"webrtc"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &body))
	require.Contains(t, body.Data.WebRTC, "/live/table-view_standard?")
}

func TestPlayURIRejectsInvalidIdentifiers(t *testing.T) {
	s := &Server{MMXBackend: "http://127.0.0.1:8890", TXSecretKeyBack: "secret"}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/playUri", s.playUriHandler)

	for _, path := range []string{
		"/api/playUri?app=../live&stream=test",
		"/api/playUri?app=live&stream=../../secret",
		"/api/playUri?app=live&stream=test%2Fother",
		"/api/playUri?app=live&stream=",
	} {
		res := httptest.NewRecorder()
		router.ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, http.StatusBadRequest, res.Code, path)
	}
}

func TestTxURLRejectsInvalidStream(t *testing.T) {
	s := &Server{TXSecretKeyBack: "secret"}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/play/txUrl", s.txUrlHandler)

	res := httptest.NewRecorder()
	router.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/play/txUrl?stream=test%2Fother", nil))
	require.Equal(t, http.StatusBadRequest, res.Code)
}

func TestPlayRateLimitIsSharedAcrossAliases(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	api := router.Group("", rateLimitMiddleware(2, time.Minute))
	api.GET("/one", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	api.GET("/two", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	for _, path := range []string{"/one", "/two", "/one"} {
		res := httptest.NewRecorder()
		router.ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil))
		if path == "/one" && res.Code == http.StatusTooManyRequests {
			require.Equal(t, "60", res.Header().Get("Retry-After"))
			return
		}
		require.Equal(t, http.StatusNoContent, res.Code)
	}
	t.Fatal("third request was not rate limited")
}

func TestAuthFirstStartupGeneratesRandomPassword(t *testing.T) {
	t.Setenv("MMXADMIN_USERNAME", "")
	t.Setenv("MMXADMIN_PASSWORD", "")
	store, err := OpenStore(filepath.Join(t.TempDir(), "admin.db3"))
	require.NoError(t, err)
	defer store.Close()

	_, err = newAuthManager(store, nil)
	require.NoError(t, err)
	username, hash := store.AdminCredentials()
	require.Equal(t, "admin", username)
	require.NotEmpty(t, hash)
}

func TestAuthPersistedCredentialsDoNotRequireEnvironment(t *testing.T) {
	t.Setenv("MMXADMIN_PASSWORD", "deployment-password")
	store, err := OpenStore(filepath.Join(t.TempDir(), "admin.db3"))
	require.NoError(t, err)
	defer store.Close()
	_, err = newAuthManager(store, nil)
	require.NoError(t, err)

	t.Setenv("MMXADMIN_PASSWORD", "")
	_, err = newAuthManager(store, nil)
	require.NoError(t, err)
}

func TestAuthLifecycle(t *testing.T) {
	t.Setenv("MMXADMIN_USERNAME", "test-admin")
	t.Setenv("MMXADMIN_PASSWORD", "initial-password")
	store, err := OpenStore(filepath.Join(t.TempDir(), "admin.db3"))
	require.NoError(t, err)
	defer store.Close()
	auth, err := newAuthManager(store, nil)
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/login", auth.login)
	router.GET("/protected", auth.middleware(), func(c *gin.Context) { respOK(c, gin.H{}) })
	router.POST("/change-password", auth.middleware(), auth.changePassword)

	request := func(method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		return res
	}

	require.Equal(t, http.StatusUnauthorized, request(http.MethodGet, "/protected", "", nil).Code)
	login := request(http.MethodPost, "/login", `{"username":"test-admin","password":"initial-password"}`, nil)
	require.Equal(t, http.StatusOK, login.Code)
	cookie := login.Result().Cookies()[0]
	require.True(t, cookie.HttpOnly)
	require.Equal(t, http.SameSiteStrictMode, cookie.SameSite)
	require.Equal(t, http.StatusOK, request(http.MethodGet, "/protected", "", cookie).Code)

	changed := request(http.MethodPost, "/change-password", `{"new_password":"updated-password"}`, cookie)
	require.Equal(t, http.StatusOK, changed.Code)
	require.Equal(t, http.StatusUnauthorized, request(http.MethodGet, "/protected", "", cookie).Code)
	require.Equal(t, http.StatusUnauthorized, request(http.MethodPost, "/login", `{"username":"test-admin","password":"initial-password"}`, nil).Code)
	require.Equal(t, http.StatusOK, request(http.MethodPost, "/login", `{"username":"test-admin","password":"updated-password"}`, nil).Code)
}
