package admin

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/logger"
)

type noopLogger struct{}

func (noopLogger) Log(logger.Level, string, ...any) {}

func writeTestConfFile(t *testing.T, ingestSources string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "record.yml")
	require.NoError(t, os.WriteFile(path, []byte(
		"logLevel: info\n"+ingestSources+"paths:\n  all:\n    source: publisher\n"), 0o644))
	return path
}

func TestDeployConfigGetReturnsCurrentValues(t *testing.T) {
	confPath := writeTestConfFile(t, "ingestSources: [\"tencent:rtmp://a\"]\n")

	s := &Server{ConfPath: confPath, Parent: noopLogger{}}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/deploy-config", s.deployConfigGetHandler)

	res := httptest.NewRecorder()
	router.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/deploy-config", nil))
	require.Equal(t, http.StatusOK, res.Code)
	require.Contains(t, res.Body.String(), `"tencent:rtmp://a"`)
}

func TestDeployConfigSetRejectsBlankIngestSource(t *testing.T) {
	s := &Server{Parent: noopLogger{}}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/deploy-config", s.deployConfigSetHandler)

	res := httptest.NewRecorder()
	router.ServeHTTP(res, httptest.NewRequest(http.MethodPost, "/deploy-config",
		strings.NewReader(`{"ingest_sources":["  "]}`)))
	require.Equal(t, http.StatusBadRequest, res.Code)
}

func TestDeployConfigSetPersistsIngestSources(t *testing.T) {
	confPath := writeTestConfFile(t, "ingestSources: [\"tencent:rtmp://old\"]\n")

	s := &Server{ConfPath: confPath, Parent: noopLogger{}}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/deploy-config", s.deployConfigSetHandler)

	res := httptest.NewRecorder()
	router.ServeHTTP(res, httptest.NewRequest(http.MethodPost, "/deploy-config",
		strings.NewReader(`{"ingest_sources":["tencent:rtmp://new"]}`)))
	require.Equal(t, http.StatusOK, res.Code)

	confContent, err := os.ReadFile(confPath)
	require.NoError(t, err)
	require.Contains(t, string(confContent), `ingestSources: ["tencent:rtmp://new"]`)
}

func TestDeployConfigSetWithoutConfPathIsUnavailable(t *testing.T) {
	s := &Server{Parent: noopLogger{}}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/deploy-config", s.deployConfigSetHandler)

	res := httptest.NewRecorder()
	router.ServeHTTP(res, httptest.NewRequest(http.MethodPost, "/deploy-config",
		strings.NewReader(`{"ingest_sources":["tencent:rtmp://new"]}`)))
	require.Equal(t, http.StatusServiceUnavailable, res.Code)
}

func TestRestartHandlerUnavailableWithoutRestartFunc(t *testing.T) {
	s := &Server{Parent: noopLogger{}}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/restart", s.restartHandler)

	res := httptest.NewRecorder()
	router.ServeHTTP(res, httptest.NewRequest(http.MethodPost, "/restart", nil))
	require.Equal(t, http.StatusServiceUnavailable, res.Code)
}

func TestRestartHandlerInvokesRestartFunc(t *testing.T) {
	var mu sync.Mutex
	called := false
	done := make(chan struct{})
	s := &Server{
		Parent: noopLogger{},
		RestartFunc: func() {
			mu.Lock()
			called = true
			mu.Unlock()
			close(done)
		},
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/restart", s.restartHandler)

	res := httptest.NewRecorder()
	router.ServeHTTP(res, httptest.NewRequest(http.MethodPost, "/restart", nil))
	require.Equal(t, http.StatusOK, res.Code)

	<-done
	mu.Lock()
	defer mu.Unlock()
	require.True(t, called)
}
