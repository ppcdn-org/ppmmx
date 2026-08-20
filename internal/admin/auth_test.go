package admin

import (
	"encoding/json"
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

func TestPlayURIUsesNodeBackendByDefault(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "admin.db3"))
	require.NoError(t, err)
	defer store.Close()
	s := &Server{MMXBackend: "http://127.0.0.1:8890", Store: store, TXSecretKeyBack: "secret"}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/playUri", s.playUriHandler)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/playUri?app=live&stream=test", nil))
	require.Equal(t, http.StatusOK, res.Code)
	var body struct {
		Data struct {
			MainPlayURI string `json:"main_play_uri"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &body))
	require.Contains(t, body.Data.MainPlayURI, "http://127.0.0.1:8890/live/test/whep?")
	playURL, err := url.Parse(body.Data.MainPlayURI)
	require.NoError(t, err)
	expires, err := strconv.ParseInt(playURL.Query().Get("txTime"), 16, 64)
	require.NoError(t, err)
	require.WithinDuration(t, time.Now().Add(playSignatureTTL), time.Unix(expires, 0), 2*time.Second)
}

func TestTxURLQualityLayers(t *testing.T) {
	s := &Server{Store: nil, TXSecretKeyBack: "secret"}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/play/txUrl", s.txUrlHandler)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/play/txUrl?stream=3drush-fwh_standard", nil))
	require.Equal(t, http.StatusOK, res.Code)
	var body struct {
		Data struct {
			Streams []struct {
				WebRTC string `json:"webrtc"`
			} `json:"streams"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &body))
	require.Len(t, body.Data.Streams, 3)
	require.Contains(t, body.Data.Streams[0].WebRTC, "/live/3drush-fwh_q0?")
	require.Contains(t, body.Data.Streams[1].WebRTC, "/live/3drush-fwh_q1?")
	require.Contains(t, body.Data.Streams[2].WebRTC, "/live/3drush-fwh_q2?")
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

func TestAuthFirstStartupRequiresPassword(t *testing.T) {
	t.Setenv("MMXADMIN_USERNAME", "")
	t.Setenv("MMXADMIN_PASSWORD", "")
	store, err := OpenStore(filepath.Join(t.TempDir(), "admin.db3"))
	require.NoError(t, err)
	defer store.Close()

	_, err = newAuthManager(store)
	require.EqualError(t, err, "MMXADMIN_PASSWORD must be set to at least 12 characters on first startup")
	username, hash := store.AdminCredentials()
	require.Empty(t, username)
	require.Empty(t, hash)
}

func TestAuthPersistedCredentialsDoNotRequireEnvironment(t *testing.T) {
	t.Setenv("MMXADMIN_PASSWORD", "deployment-password")
	store, err := OpenStore(filepath.Join(t.TempDir(), "admin.db3"))
	require.NoError(t, err)
	defer store.Close()
	_, err = newAuthManager(store)
	require.NoError(t, err)

	t.Setenv("MMXADMIN_PASSWORD", "")
	_, err = newAuthManager(store)
	require.NoError(t, err)
}

func TestAuthLifecycle(t *testing.T) {
	t.Setenv("MMXADMIN_USERNAME", "test-admin")
	t.Setenv("MMXADMIN_PASSWORD", "initial-password")
	store, err := OpenStore(filepath.Join(t.TempDir(), "admin.db3"))
	require.NoError(t, err)
	defer store.Close()
	auth, err := newAuthManager(store)
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
