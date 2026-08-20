package admin

import (
	"crypto/md5"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/bluenviron/mediamtx/internal/logger"
)

//go:embed web/*
var webAssets embed.FS

//go:embed mmxplayer/*
var mmxplayerAssets embed.FS

// Server is the admin HTTP server.
type Server struct {
	Listen          string
	MMXBackend      string
	MMXAPI          string
	NodeName        string
	Store           *Store
	Parent          logger.Writer
	TXSecretKeyBack string
	// Version is reported by the health endpoints (e.g. "v1.19.1rc001").
	Version string
	// PlayURIRateLimit caps /api/playUri and /api/play(Tx)?Url requests per
	// source IP per minute. Zero (the default) means 30.
	PlayURIRateLimit int

	httpServer *http.Server
}

func respOK(c *gin.Context, data any) {
	c.JSON(http.StatusOK, gin.H{"code": http.StatusOK, "msg": "success", "data": data})
}

func respErr(c *gin.Context, code int, msg string, data any) {
	c.JSON(code, gin.H{"code": code, "msg": msg, "data": data})
}

type rateEntry struct {
	window time.Time
	count  int
}

func rateLimitMiddleware(limit int, window time.Duration) gin.HandlerFunc {
	var mu sync.Mutex
	entries := make(map[string]rateEntry)
	return func(c *gin.Context) {
		ip, _, err := net.SplitHostPort(c.Request.RemoteAddr)
		if err != nil {
			ip = c.Request.RemoteAddr
		}
		if ip == "" {
			ip = "unknown"
		}
		now := time.Now()
		mu.Lock()
		entry := entries[ip]
		if entry.window.IsZero() || now.Sub(entry.window) >= window {
			entry = rateEntry{window: now}
		}
		entry.count++
		entries[ip] = entry
		if len(entries) > 1000 {
			oldestKey := ""
			var oldest time.Time
			for k, item := range entries {
				if now.Sub(item.window) >= window {
					delete(entries, k)
					continue
				}
				if oldest.IsZero() || item.window.Before(oldest) {
					oldestKey, oldest = k, item.window
				}
			}
			if len(entries) > 1000 {
				delete(entries, oldestKey)
			}
		}
		limited := entry.count > limit
		mu.Unlock()
		if limited {
			c.Header("Retry-After", strconv.Itoa(int(window/time.Second)))
			respErr(c, http.StatusTooManyRequests, "rate limit exceeded", "")
			c.Abort()
			return
		}
		c.Next()
	}
}

const playSignatureTTL = 5 * time.Minute

var playIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func validPlayIdentifier(value string) bool {
	return playIdentifierPattern.MatchString(value)
}

func (s *Server) signedPlayParams(stream string) string {
	txTime := fmt.Sprintf("%X", time.Now().Add(playSignatureTTL).Unix())
	txSecret := fmt.Sprintf("%x", md5.Sum([]byte(s.TXSecretKeyBack+stream+txTime)))
	return fmt.Sprintf("txTime=%s&txSecret=%s", txTime, txSecret)
}

func mmxGetAPI(apiURL, path string) (map[string]any, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(apiURL + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, &upstreamError{status: resp.StatusCode, message: strings.TrimSpace(string(body))}
	}
	var data map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&data); err != nil {
		return nil, fmt.Errorf("invalid JSON from upstream: %w", err)
	}
	return data, nil
}

type upstreamError struct {
	status  int
	message string
}

func (e *upstreamError) Error() string { return e.message }

func writeUpstreamError(c *gin.Context, err error) {
	if ue, ok := err.(*upstreamError); ok {
		respErr(c, ue.status, ue.message, "")
		return
	}
	respErr(c, http.StatusBadGateway, err.Error(), "")
}

// ── Handlers ──────────────────────────────────────────────────

func (s *Server) nodeHandler(c *gin.Context) {
	info, errInfo := mmxGetAPI(s.MMXAPI, "/v3/info")
	config, _ := mmxGetAPI(s.MMXAPI, "/v3/config/global/get")
	paths, _ := mmxGetAPI(s.MMXAPI, "/v3/config/paths/list")

	node := gin.H{
		"name":      s.NodeName,
		"reachable": errInfo == nil,
		"version":   "-",
		"started":   "-",
	}
	if info != nil {
		if v, ok := info["version"].(string); ok {
			node["version"] = v
		}
		if st, ok := info["started"].(string); ok {
			node["started"] = st
		}
	}
	respOK(c, gin.H{
		"node":   node,
		"info":   info,
		"config": config,
		"paths":  paths,
	})
}

func (s *Server) nodesHandler(c *gin.Context) {
	info, errInfo := mmxGetAPI(s.MMXAPI, "/v3/info")
	node := gin.H{
		"name":      s.NodeName,
		"reachable": errInfo == nil,
		"version":   "-",
		"started":   "-",
	}
	if info != nil {
		if v, ok := info["version"].(string); ok {
			node["version"] = v
		}
		if st, ok := info["started"].(string); ok {
			node["started"] = st
		}
	}
	respOK(c, gin.H{"nodes": []gin.H{node}})
}

func (s *Server) streamsHandler(c *gin.Context) {
	data, err := mmxGetAPI(s.MMXAPI, "/v3/paths/list")
	if err != nil {
		writeUpstreamError(c, err)
		return
	}
	items, _ := data["items"].([]any)
	if items == nil {
		items = []any{}
	}
	respOK(c, gin.H{"streams": items})
}

func (s *Server) sessionsHandler(c *gin.Context) {
	data, err := mmxGetAPI(s.MMXAPI, "/v3/webrtcsessions/list")
	if err != nil {
		writeUpstreamError(c, err)
		return
	}
	items, _ := data["items"].([]any)
	if items == nil {
		items = []any{}
	}
	respOK(c, gin.H{"sessions": items})
}

func (s *Server) kickSessionHandler(c *gin.Context) {
	uuid := c.Param("uuid")
	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequest(http.MethodPost, s.MMXAPI+"/v3/webrtcsessions/kick/"+uuid, nil)
	resp, err := client.Do(req)
	if err != nil {
		respErr(c, http.StatusBadGateway, err.Error(), "")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		respErr(c, resp.StatusCode, strings.TrimSpace(string(body)), "")
		return
	}
	respOK(c, gin.H{"message": "session kicked", "uuid": uuid})
}

func (s *Server) configHandler(c *gin.Context) {
	global, _ := mmxGetAPI(s.MMXAPI, "/v3/config/global/get")
	paths, _ := mmxGetAPI(s.MMXAPI, "/v3/config/paths/list")
	respOK(c, gin.H{"global": global, "paths": paths})
}

func (s *Server) ingestStatsHandler(c *gin.Context) {
	data, err := mmxGetAPI(s.MMXAPI, "/v3/paths/list")
	if err != nil {
		writeUpstreamError(c, err)
		return
	}
	items, _ := data["items"].([]any)
	if items == nil {
		items = []any{}
	}
	respOK(c, gin.H{"items": items})
}

func (s *Server) gameStatsHandler(c *gin.Context) {
	respOK(c, gin.H{"items": []any{}})
}

func (s *Server) siteConfigHandler(c *gin.Context) {
	mainDomain, backupDomain := s.defaultPlayerDomain(), "play.numericgame.ph"
	env := "test"
	if s.Store != nil {
		mainDomain, backupDomain = s.Store.PlayerConfig(mainDomain)
		env = s.Store.VideoStatEnv()
	}
	global, _ := mmxGetAPI(s.MMXAPI, "/v3/config/global/get")
	paths, _ := mmxGetAPI(s.MMXAPI, "/v3/config/paths/list")
	configs := []adminSiteStreamConfig{}
	if s.Store != nil {
		stored, err := s.Store.SiteStreamConfigs()
		if err != nil {
			respErr(c, http.StatusInternalServerError, err.Error(), "")
			return
		}
		for _, item := range stored {
			configs = append(configs, adminSiteStreamConfig{
				ID: item.ID, SiteName: item.SiteName, StreamName: item.StreamName, ViewName: item.ViewName,
				GeneratedSite:   "studio_" + item.SiteName,
				GeneratedStream: "live/" + item.StreamName + "-" + item.ViewName,
			})
		}
	}
	respOK(c, gin.H{
		"main_player_domain":   mainDomain,
		"backup_player_domain": backupDomain,
		"videostat_env":        env,
		"videostat_api":        VideoStatEnvs[env],
		"global":               global,
		"paths":                paths,
		"site_stream_configs":  configs,
	})
}

type adminSiteStreamConfig struct {
	ID              int64  `json:"id"`
	SiteName        string `json:"site_name"`
	StreamName      string `json:"stream_name"`
	ViewName        string `json:"view_name"`
	GeneratedSite   string `json:"generated_site"`
	GeneratedStream string `json:"generated_stream"`
}

func (s *Server) siteStreamConfigSetHandler(c *gin.Context) {
	if s.Store == nil {
		respErr(c, http.StatusInternalServerError, "store not available", "")
		return
	}
	var req struct {
		Configs []adminSiteStreamConfig `json:"configs"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respErr(c, http.StatusBadRequest, err.Error(), "")
		return
	}
	configs := make([]SiteStreamConfig, 0, len(req.Configs))
	seen := make(map[string]struct{})
	for _, item := range req.Configs {
		item.SiteName = strings.TrimSpace(item.SiteName)
		item.StreamName = strings.TrimSpace(item.StreamName)
		item.ViewName = strings.TrimSpace(item.ViewName)
		if item.SiteName == "" || item.StreamName == "" || item.ViewName == "" {
			respErr(c, http.StatusBadRequest, "site_name, stream_name and view_name are required", "")
			return
		}
		key := item.SiteName + "\x00" + item.StreamName + "\x00" + item.ViewName
		if _, ok := seen[key]; ok {
			respErr(c, http.StatusBadRequest, "duplicate site stream view configuration", "")
			return
		}
		seen[key] = struct{}{}
		configs = append(configs, SiteStreamConfig{SiteName: item.SiteName, StreamName: item.StreamName, ViewName: item.ViewName})
	}
	if err := s.Store.SetSiteStreamConfigs(configs); err != nil {
		respErr(c, http.StatusInternalServerError, err.Error(), "")
		return
	}
	respOK(c, gin.H{"message": "ok"})
}

func (s *Server) playerHandler(c *gin.Context) {
	mainDomain, backupDomain := s.defaultPlayerDomain(), "play.numericgame.ph"
	if s.Store != nil {
		mainDomain, backupDomain = s.Store.PlayerConfig(mainDomain)
	}
	respOK(c, gin.H{"players": []gin.H{
		{"name": s.NodeName, "type": "primary", "host": mainDomain, "url": "/simulcast/", "desc": "Simulcast main-play-uri player"},
		{"name": "backup", "type": "backup", "host": backupDomain, "url": "/multitrack/", "desc": "Tencent Cloud multitrack play"},
	}})
}

func (s *Server) playerConfigGetHandler(c *gin.Context) {
	mainDomain, backupDomain := s.defaultPlayerDomain(), "play.numericgame.ph"
	env := "test"
	if s.Store != nil {
		mainDomain, backupDomain = s.Store.PlayerConfig(mainDomain)
		env = s.Store.VideoStatEnv()
	}
	respOK(c, gin.H{
		"main_player_domain":   mainDomain,
		"backup_player_domain": backupDomain,
		"videostat_env":        env,
		"videostat_environments": map[string]string{
			"test": "https://lotto-videostat.gelotto-test.com/api/stat",
			"uat":  "https://lotto-videostat.gelotto-uat.com/api/stat",
			"stag": "https://lotto-videostat.numericgame.io/api/stat",
			"prod": "https://lotto-videostat.numericgame.ph/api/stat",
		},
	})
}

func (s *Server) playerConfigSetHandler(c *gin.Context) {
	var req struct {
		MainPlayerDomain   string `json:"main_player_domain"`
		BackupPlayerDomain string `json:"backup_player_domain"`
		VideoStatEnv       string `json:"videostat_env"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respErr(c, http.StatusBadRequest, err.Error(), "")
		return
	}
	if s.Store == nil {
		respErr(c, http.StatusInternalServerError, "store not available", "")
		return
	}
	if err := s.Store.SetPlayerConfig(req.MainPlayerDomain, req.BackupPlayerDomain); err != nil {
		respErr(c, http.StatusInternalServerError, err.Error(), "")
		return
	}
	if req.VideoStatEnv != "" {
		if err := s.Store.SetVideoStatEnv(req.VideoStatEnv); err != nil {
			respErr(c, http.StatusInternalServerError, err.Error(), "")
			return
		}
	}
	s.Parent.Log(logger.Info, "[admin] player config updated: main=%s backup=%s videostat_env=%s", req.MainPlayerDomain, req.BackupPlayerDomain, req.VideoStatEnv)
	respOK(c, gin.H{"message": "ok"})
}

func (s *Server) downloadsHandler(c *gin.Context) {
	respOK(c, gin.H{"obs": gin.H{
		"name":    "OBS Studio 32.1.2 Patched",
		"version": "32.1.2-patched",
		"url":     "https://github.com/Elon666-ai/obs32.1.2patched/releases/latest",
		"sha256":  "EC7E5A46E5EFEBBB365B9958EB4EF55E95AF9E96F09EC5C690D9426213DB0B6A",
	}})
}

func (s *Server) txUrlHandler(c *gin.Context) {
	if s.TXSecretKeyBack == "" {
		c.JSON(http.StatusOK, gin.H{"code": -1, "data": gin.H{}})
		return
	}
	stream := c.DefaultQuery("stream", "")
	if !validPlayIdentifier(stream) {
		respErr(c, http.StatusBadRequest, "invalid stream parameter", "")
		return
	}
	backupHost := "play.numericgame.ph"
	if s.Store != nil {
		_, backupHost = s.Store.PlayerConfig("")
	}
	genUrl := func(streamKey string) string {
		return "webrtc://" + backupHost + "/live/" + streamKey + "?" + s.signedPlayParams(streamKey)
	}
	// A quality stream is already a complete Tencent multitrack key.
	if strings.HasSuffix(stream, "_q0") || strings.HasSuffix(stream, "_q1") || strings.HasSuffix(stream, "_q2") {
		c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"webrtc": genUrl(stream)}})
		return
	}
	stream = strings.TrimSuffix(stream, "_standard")
	stream = strings.TrimSuffix(stream, "_economic")
	stream = strings.TrimSuffix(stream, "_audio")
	if !strings.Contains(stream, "_q") {
		streams := []gin.H{
			{"id": 0, "label": "1080p", "rid": "q0", "bitrate": 2600000, "webrtc": genUrl(stream + "_q0")},
			{"id": 1, "label": "720p", "rid": "q1", "bitrate": 1734000, "webrtc": genUrl(stream + "_q1")},
			{"id": 2, "label": "360p", "rid": "q2", "bitrate": 868000, "webrtc": genUrl(stream + "_q2")},
		}
		c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"streams": streams}})
		return
	}

	uri := genUrl(stream)
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"webrtc": uri}})
}

func (s *Server) videoStatEnvHandler(c *gin.Context) {
	env := "test"
	if s.Store != nil {
		env = s.Store.VideoStatEnv()
	}
	respOK(c, gin.H{"env": env, "baseUrl": VideoStatEnvs[env]})
}

func (s *Server) playUriHandler(c *gin.Context) {
	if s.TXSecretKeyBack == "" {
		respErr(c, http.StatusServiceUnavailable, "TX_SecretKey_Back is not configured", "")
		return
	}
	app := c.DefaultQuery("app", "live")
	stream := c.DefaultQuery("stream", "")
	if !validPlayIdentifier(app) {
		respErr(c, http.StatusBadRequest, "invalid app parameter", "")
		return
	}
	if !validPlayIdentifier(stream) {
		respErr(c, http.StatusBadRequest, "invalid stream parameter", "")
		return
	}

	mainDomain, backupHost := s.defaultPlayerDomain(), "play.numericgame.ph"
	if s.Store != nil {
		mainDomain, backupHost = s.Store.PlayerConfig(mainDomain)
	}

	txParams := s.signedPlayParams(stream)

	mainURI := fmt.Sprintf("http://%s/%s/%s/whep?%s", mainDomain, app, stream, txParams)
	backupURI := fmt.Sprintf("webrtc://%s/%s/%s?%s",
		backupHost, app, stream, txParams)

	videoStatAPI := VideoStatEnvs["test"]
	if s.Store != nil {
		if u, ok := VideoStatEnvs[s.Store.VideoStatEnv()]; ok {
			videoStatAPI = u
		}
	}

	respOK(c, gin.H{
		"main_play_uri":   mainURI,
		"backup_play_uri": backupURI,
		"video_stat_api":  videoStatAPI,
	})
}

func (s *Server) defaultPlayerDomain() string {
	target, err := url.Parse(s.MMXBackend)
	if err == nil && target.Host != "" {
		return target.Host
	}
	return "127.0.0.1:8889"
}

func (s *Server) healthHandler(c *gin.Context) {
	version := s.Version
	if version == "" {
		version = "unknown"
	}
	c.JSON(http.StatusOK, gin.H{
		"code": 0, "msg": "success",
		"data": gin.H{"app": "mmx", "version": version},
	})
}

func fileServerFrom(assets embed.FS, root string) gin.HandlerFunc {
	sub, _ := fs.Sub(assets, root)
	srv := http.FileServer(http.FS(sub))
	return func(c *gin.Context) {
		c.Request.URL.Path = c.Param("path")
		srv.ServeHTTP(c.Writer, c.Request)
	}
}

func adminRootHandler(c *gin.Context) {
	c.Redirect(http.StatusMovedPermanently, "/dashboard")
}

func adminDashboardHandler(c *gin.Context) {
	sub, _ := fs.Sub(webAssets, "web")
	f, _ := sub.Open("index.html")
	if f == nil {
		c.String(http.StatusNotFound, "not found")
		return
	}
	defer f.Close()
	c.DataFromReader(http.StatusOK, -1, "text/html; charset=utf-8", f, nil)
}

// Initialize builds the Gin router and starts listening.
func (s *Server) Initialize() error {
	auth, err := newAuthManager(s.Store)
	if err != nil {
		return fmt.Errorf("initialize admin authentication: %w", err)
	}
	target, err := url.Parse(s.MMXBackend)
	if err != nil {
		return fmt.Errorf("invalid mmx_backend: %w", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "POST, OPTIONS, GET, DELETE, PUT, PATCH")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	})

	// Admin API
	r.GET("/admin/api/v1/health", s.healthHandler)
	r.POST("/admin/api/v1/auth/login", auth.login)
	protected := r.Group("/admin/api/v1", auth.middleware())
	protected.GET("/auth/me", auth.me)
	protected.POST("/auth/logout", auth.logout)
	protected.POST("/auth/change-password", auth.changePassword)
	protected.GET("/node", s.nodeHandler)
	protected.GET("/nodes", s.nodesHandler)
	protected.GET("/nodes/:name", s.nodeHandler)
	protected.GET("/streams", s.streamsHandler)
	protected.GET("/sessions", s.sessionsHandler)
	protected.DELETE("/sessions/:uuid", s.kickSessionHandler)
	protected.GET("/config", s.configHandler)
	protected.GET("/ingest-stats", s.ingestStatsHandler)
	protected.GET("/game-stats", s.gameStatsHandler)
	protected.GET("/site-config", s.siteConfigHandler)
	protected.POST("/site-stream-config", s.siteStreamConfigSetHandler)
	protected.GET("/player", s.playerHandler)
	protected.GET("/player-config", s.playerConfigGetHandler)
	protected.POST("/player-config", s.playerConfigSetHandler)
	protected.GET("/downloads", s.downloadsHandler)

	// Play URI API
	playURIRateLimit := s.PlayURIRateLimit
	if playURIRateLimit <= 0 {
		playURIRateLimit = 30
	}
	api := r.Group("/api")
	playAPI := api.Group("", rateLimitMiddleware(playURIRateLimit, time.Minute))
	playAPI.GET("/playUri", s.playUriHandler)
	playAPI.GET("/play/txUrl", s.txUrlHandler)
	playAPI.GET("/playTxUrl", s.txUrlHandler)
	r.GET("/api/settings/app-env", s.videoStatEnvHandler)

	// Dashboard
	r.GET("/", adminRootHandler)
	r.GET("/dashboard", adminDashboardHandler)

	// Players
	r.Any("/simulcast", func(c *gin.Context) { c.Redirect(http.StatusMovedPermanently, "/simulcast/") })
	r.Any("/simulcast/*path", fileServerFrom(mmxplayerAssets, "mmxplayer/simulcast"))
	r.Any("/multitrack", func(c *gin.Context) { c.Redirect(http.StatusMovedPermanently, "/multitrack/") })
	r.Any("/multitrack/*path", fileServerFrom(mmxplayerAssets, "mmxplayer/multitrack"))

	// Legacy player paths
	r.Any("/mmxplayer", func(c *gin.Context) { c.Redirect(http.StatusMovedPermanently, "/simulcast/") })
	r.Any("/mmxplayer/simulcast/*path", func(c *gin.Context) {
		c.Redirect(http.StatusMovedPermanently, "/simulcast/"+strings.TrimPrefix(c.Param("path"), "/"))
	})
	r.Any("/mmxplayer/multitrack/*path", func(c *gin.Context) {
		c.Redirect(http.StatusMovedPermanently, "/multitrack/"+strings.TrimPrefix(c.Param("path"), "/"))
	})

	// Legacy
	r.GET("/lotto/health", s.healthHandler)
	api.POST("/split-rec", func(c *gin.Context) { proxy.ServeHTTP(c.Writer, c.Request) })
	r.GET("/test/token", func(c *gin.Context) {
		c.String(http.StatusOK, "time=%d&token=not-implemented", time.Now().Unix()+180)
	})
	r.GET("/test/txUrl", func(c *gin.Context) { respOK(c, gin.H{}) })

	// Catch-all → MMX
	r.NoRoute(func(c *gin.Context) {
		// Validate txTime/txSecret only on WHEP (playback), not WHIP (publish)
		if strings.HasSuffix(c.Request.URL.Path, "/whep") {
			if s.TXSecretKeyBack == "" {
				respErr(c, http.StatusServiceUnavailable, "TX_SecretKey_Back is not configured", "")
				return
			}
			txTime := c.Query("txTime")
			txSecret := c.Query("txSecret")
			if txTime == "" || txSecret == "" {
				respErr(c, http.StatusUnauthorized, "missing txTime or txSecret", "")
				return
			}
			ts, err := strconv.ParseInt(txTime, 16, 64)
			if err != nil || time.Now().Unix() > ts {
				respErr(c, http.StatusUnauthorized, "txTime expired", "")
				return
			}
			// Extract stream key from path: /app/stream/whep
			parts := strings.Split(strings.Trim(c.Request.URL.Path, "/"), "/")
			var streamKey string
			for i, p := range parts {
				if p == "whep" || p == "whip" {
					if i >= 2 {
						streamKey = parts[i-1]
					}
					break
				}
			}
			expected := fmt.Sprintf("%x", md5.Sum([]byte(s.TXSecretKeyBack+streamKey+txTime)))
			if txSecret != expected {
				respErr(c, http.StatusUnauthorized, "invalid txSecret", "")
				return
			}
		}
		proxy.ServeHTTP(c.Writer, c.Request)
	})

	listener, err := net.Listen("tcp", s.Listen)
	if err != nil {
		return fmt.Errorf("listen admin on %s: %w", s.Listen, err)
	}
	s.httpServer = &http.Server{Addr: s.Listen, Handler: r.Handler()}
	go func() {
		s.Parent.Log(logger.Info, "[admin] starting on %s", s.Listen)
		if err := s.httpServer.Serve(listener); err != http.ErrServerClosed {
			s.Parent.Log(logger.Warn, "[admin] %v", err)
		}
	}()
	return nil
}

// Close shuts down the admin server.
func (s *Server) Close() {
	if s.httpServer != nil {
		s.httpServer.Close()
	}
}
