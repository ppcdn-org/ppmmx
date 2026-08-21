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
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/bluenviron/mediamtx/internal/conf"
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
	// BackupPlayerDomain is the fallback backup player domain used when the
	// admin store has none configured. Read from BACKUP_PLAYER_DOMAIN in
	// Initialize() if this field is left empty; falls back to the main
	// domain itself if neither is set.
	BackupPlayerDomain string
	// Version is reported by the health endpoints (e.g. "v1.19.1rc002").
	Version string
	// PlayURIRateLimit caps /api/playUri and /api/play(Tx)?Url requests per
	// source IP per minute. Zero (the default) means 30.
	PlayURIRateLimit int
	// PlaySignatureTTL controls how long a signed play URL's txTime/txSecret
	// stays valid. Zero (the default) means 1h.
	PlaySignatureTTL time.Duration
	// ConfPath is the currently loaded YAML config file, used to read and
	// patch ingestSources. Deploy settings are unavailable if empty (e.g.
	// no config file was found at startup).
	ConfPath string
	// RestartFunc, if set, is called to restart the backend process after
	// a deploy-config change (ingestSources) that only takes effect on the
	// next boot. Runs in a goroutine so the triggering HTTP response can
	// be written first.
	RestartFunc func()

	httpServer *http.Server
	// v3Handler, when set, serves the Control API /v3/* routes directly on
	// the admin's gin engine instead of needing a separate :9997 listener.
	v3Handler http.Handler
}

// SetV3Handler attaches a Control API handler to be mounted at /v3/* on
// the admin port, removing the need for a standalone API HTTP listener.
func (s *Server) SetV3Handler(h http.Handler) { s.v3Handler = h }

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

const defaultPlaySignatureTTL = time.Hour

var playIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func validPlayIdentifier(value string) bool {
	return playIdentifierPattern.MatchString(value)
}

func (s *Server) signedPlayParams(stream string) string {
	ttl := s.PlaySignatureTTL
	if ttl <= 0 {
		ttl = defaultPlaySignatureTTL
	}
	txTime := fmt.Sprintf("%X", time.Now().Add(ttl).Unix())
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
	mainDomain, backupDomain := s.mainPlayerDomain(c)
	env := "test"
	if s.Store != nil {
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
	mainDomain, backupDomain := s.mainPlayerDomain(c)
	respOK(c, gin.H{"players": []gin.H{
		{"name": s.NodeName, "type": "primary", "host": mainDomain, "url": "/simulcast/", "desc": "Simulcast main-play-uri player"},
		{"name": "backup", "type": "backup", "host": backupDomain, "url": "/multitrack/", "desc": "Tencent Cloud multitrack play"},
	}})
}

func (s *Server) playerConfigGetHandler(c *gin.Context) {
	mainDomain, backupDomain := s.mainPlayerDomain(c)
	env := "test"
	if s.Store != nil {
		env = s.Store.VideoStatEnv()
	}
	respOK(c, gin.H{
		"main_player_domain":   mainDomain,
		"backup_player_domain": backupDomain,
		"videostat_env":          env,
		"videostat_environments": VideoStatEnvs,
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
		"url":     "https://github.com/Elon666-ai/obs32.1.2patched/releases",
	}})
}

func (s *Server) deployConfigGetHandler(c *gin.Context) {
	var ingestSources []string
	if s.ConfPath != "" {
		var err error
		ingestSources, err = conf.ReadIngestSources(s.ConfPath)
		if err != nil {
			respErr(c, http.StatusInternalServerError, err.Error(), "")
			return
		}
	}
	if ingestSources == nil {
		ingestSources = []string{}
	}
	respOK(c, gin.H{
		"ingest_sources": ingestSources,
		"conf_path":      s.ConfPath,
	})
}

func (s *Server) deployConfigSetHandler(c *gin.Context) {
	var req struct {
		IngestSources []string `json:"ingest_sources"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respErr(c, http.StatusBadRequest, err.Error(), "")
		return
	}
	for _, src := range req.IngestSources {
		if strings.TrimSpace(src) == "" {
			respErr(c, http.StatusBadRequest, "ingest_sources entries must not be blank", "")
			return
		}
	}

	if s.ConfPath == "" {
		respErr(c, http.StatusServiceUnavailable, "no config file loaded, cannot save ingest_sources", "")
		return
	}
	if err := conf.PatchIngestSources(s.ConfPath, req.IngestSources); err != nil {
		respErr(c, http.StatusInternalServerError, err.Error(), "")
		return
	}

	s.Parent.Log(logger.Info, "[admin] deploy config updated: ingest_sources=%v (restart required)", req.IngestSources)
	respOK(c, gin.H{"message": "saved, restart required to take effect"})
}

func (s *Server) restartHandler(c *gin.Context) {
	if s.RestartFunc == nil {
		respErr(c, http.StatusServiceUnavailable, "restart is not supported on this deployment", "")
		return
	}
	s.Parent.Log(logger.Info, "[admin] restart requested via admin API")
	respOK(c, gin.H{"message": "restarting"})
	go s.RestartFunc()
}

// tencentLayerLadder describes the fixed quality ladder returned by
// txUrlHandler when given a bare stream name, in tencentLayerSuffixes
// order (index 0 = highest resolution/"" suffix).
var tencentLayerLadder = []struct {
	Label   string
	Bitrate int
}{
	{"1080p", 2600000},
	{"720p", 1734000},
	{"360p", 868000},
	{"180p", 600000},
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
	backupHost := s.BackupPlayerDomain
	if s.Store != nil {
		_, backupHost = s.Store.PlayerConfig("", s.BackupPlayerDomain)
	}
	genUrl := func(streamKey string) string {
		return "webrtc://" + backupHost + "/live/" + streamKey + "?" + s.signedPlayParams(streamKey)
	}
	// A quality stream is already a complete Tencent multitrack key (see
	// internal/forward/layers.go's layerSuffixes for the suffixes
	// forwardTencent actually produces).
	for _, suffix := range tencentLayerSuffixes {
		if suffix != "" && strings.HasSuffix(stream, suffix) {
			c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"webrtc": genUrl(stream)}})
			return
		}
	}
	base := strings.TrimSuffix(stream, "_audio")
	if base == stream {
		streams := make([]gin.H, len(tencentLayerLadder))
		for i, layer := range tencentLayerLadder {
			suffix := tencentLayerSuffix(i)
			streams[i] = gin.H{
				"id": i, "label": layer.Label, "rid": fmt.Sprintf("q%d", i),
				"bitrate": layer.Bitrate, "webrtc": genUrl(stream + suffix),
			}
		}
		c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"streams": streams}})
		return
	}

	// "_audio" is a logical audio-only alias, not a real Tencent stream
	// key - fall back to the lowest-bandwidth real layer.
	uri := genUrl(base + tencentLayerSuffix(len(tencentLayerLadder)-1))
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"webrtc": uri}})
}

func (s *Server) videoStatEnvHandler(c *gin.Context) {
	env := "test"
	if s.Store != nil {
		env = s.Store.VideoStatEnv()
	}
	respOK(c, gin.H{"env": env, "baseUrl": VideoStatEnvs[env]})
}

// tencentLayerSuffixes maps quality layer index (0 = highest resolution) to
// its Tencent stream key suffix, mirroring internal/forward/layers.go's
// convention for the streams forwardTencent actually produces (kept as a
// separate copy since importing internal/forward here would pull in its
// WebRTC/WHIP client dependencies just for this one constant).
var tencentLayerSuffixes = [...]string{"", "_standard", "_economic", "_lite"}

func tencentLayerSuffix(i int) string {
	if i < len(tencentLayerSuffixes) {
		return tencentLayerSuffixes[i]
	}
	return fmt.Sprintf("_q%d", i)
}

// simulcastLayerCount asks mmx's own Control API how many H264 video layers
// path currently has live (0 if the path doesn't exist or isn't online -
// e.g. a table with no active publisher), and whether its matched path
// configuration has forwardTencent enabled. Best-effort: any upstream
// error is treated the same as "not currently live" (0 layers) rather than
// failing the whole request, since a path just not being live yet/right
// now is an expected, common case for this endpoint.
func (s *Server) simulcastLayerCount(app, stream string) (layers int, forwardTencent bool) {
	pathData, err := mmxGetAPI(s.MMXAPI, "/v3/paths/get/"+app+"/"+stream)
	if err != nil {
		return 0, false
	}
	tracks, _ := pathData["tracks2"].([]any)
	for _, t := range tracks {
		track, ok := t.(map[string]any)
		if !ok {
			continue
		}
		if codec, _ := track["codec"].(string); codec == "H264" {
			layers++
		}
	}
	if layers == 0 {
		return 0, false
	}

	confName, _ := pathData["confName"].(string)
	if confName == "" {
		return layers, false
	}
	confData, err := mmxGetAPI(s.MMXAPI, "/v3/config/paths/get/"+confName)
	if err != nil {
		return layers, false
	}
	forwardTencent, _ = confData["forwardTencent"].(bool)
	return layers, forwardTencent
}

func (s *Server) playUriHandler(c *gin.Context) {
	if s.TXSecretKeyBack == "" {
		respErr(c, http.StatusServiceUnavailable, "TX_SECRET_KEY_BACK is not configured", "")
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

	mainDomain, backupHost := s.mainPlayerDomain(c)
	layers, forwardTencent := s.simulcastLayerCount(app, stream)

	// Main player is on the same LAN/trusted network as mmx (studio/local
	// playback) and reaches it directly over WebRTC/HTTP, so it doesn't
	// need the txTime/txSecret signature backup URLs (public-facing
	// Tencent multitrack addresses) require; and it must always use
	// webrtcAddress's port (see mainPlayerDomain), not whatever port the
	// admin request itself came in on. All quality keys share the same
	// URL: mmx's WHEP endpoint negotiates simulcast layers itself (the
	// player switches layers over the ABR WebSocket control channel, not
	// by requesting a different URL) - the point of returning multiple
	// keys here is only to tell the player how many layers are available
	// to choose from.
	main := gin.H{}
	backup := gin.H{}
	if layers > 0 {
		mainURI := fmt.Sprintf("http://%s/%s/%s/whep", mainDomain, app, stream)
		for i := range layers {
			main[fmt.Sprintf("q%d", i)] = mainURI
		}

		// backup (Tencent) URLs, in contrast, are one real streamKey per
		// layer (forwardTencent forwards each simulcast layer to Tencent
		// under its own suffixed key - see internal/forward/layers.go),
		// so each quality key gets its own signed URL. Only present when
		// this path is actually configured to forward to Tencent -
		// otherwise there's nothing playable at any Tencent-side key.
		if forwardTencent {
			for i := range layers {
				layerStream := stream + tencentLayerSuffix(i)
				backup[fmt.Sprintf("q%d", i)] = fmt.Sprintf("webrtc://%s/%s/%s?%s",
					backupHost, app, layerStream, s.signedPlayParams(layerStream))
			}
		}
	}

	videoStatAPI := VideoStatEnvs["test"]
	if s.Store != nil {
		if u, ok := VideoStatEnvs[s.Store.VideoStatEnv()]; ok {
			videoStatAPI = u
		}
	}

	data := gin.H{
		"main":           main,
		"video_stat_api": videoStatAPI,
	}
	if forwardTencent {
		data["backup"] = backup
	}
	respOK(c, data)
}

// webrtcPort extracts the port mmx's WebRTC/HTTP server (webrtcAddress in
// origin.yml/record.yml) is actually listening on, from s.MMXBackend
// ("http://127.0.0.1:<webrtcAddress port>", derived at startup - see
// internal/core/core.go). Falls back to "8889" (the default webrtcAddress)
// if MMXBackend is somehow unset/unparsable, so callers always get a port
// rather than an empty string.
func (s *Server) webrtcPort() string {
	if target, err := url.Parse(s.MMXBackend); err == nil {
		if _, port, err := net.SplitHostPort(target.Host); err == nil && port != "" {
			return port
		}
	}
	return "8889"
}

// mainPlayerDomain returns the "host:port" clients should use to reach mmx's
// WebRTC/HTTP backend directly for main-player (studio/LAN) playback, plus
// the backup (Tencent multitrack) domain. The host is either an admin-set
// main_player_domain override or the current request's Host header; either
// way its port, if any, is discarded and replaced with webrtcAddress's own
// port (see webrtcPort) - never the port the admin request itself came in
// on, since mmx's WHIP/WHEP endpoints live on a separate listener from the
// admin API.
func (s *Server) mainPlayerDomain(c *gin.Context) (mainDomain, backupDomain string) {
	host := c.Request.Host
	if host == "" {
		host = "127.0.0.1"
	}
	backupDomain = s.BackupPlayerDomain
	if s.Store != nil {
		if stored := s.Store.Get("main_player_domain"); stored != "" {
			host = stored
		}
		_, backupDomain = s.Store.PlayerConfig("", s.BackupPlayerDomain)
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return host + ":" + s.webrtcPort(), backupDomain
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
	// Third-party video-stat reporting is deployment-specific and off by
	// default (see VideoStatEnvs) - populate from VIDEOSTAT_URL_<ENV> if set.
	for env := range VideoStatEnvs {
		if v := os.Getenv("VIDEOSTAT_URL_" + strings.ToUpper(env)); v != "" {
			VideoStatEnvs[env] = v
		}
	}
	if s.BackupPlayerDomain == "" {
		s.BackupPlayerDomain = os.Getenv("BACKUP_PLAYER_DOMAIN")
	}

	auth, err := newAuthManager(s.Store, s.Parent)
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
	protected.GET("/deploy-config", s.deployConfigGetHandler)
	protected.POST("/deploy-config", s.deployConfigSetHandler)
	protected.POST("/restart", s.restartHandler)

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

	// Legacy health-check path some deployments' load balancers/monitoring
	// already point at. Off by default; set LEGACY_HEALTH_PATH (e.g.
	// "/lotto/health") to register it without a code change.
	if p := os.Getenv("LEGACY_HEALTH_PATH"); p != "" {
		r.GET(p, s.healthHandler)
	}
	api.POST("/split-rec", func(c *gin.Context) { proxy.ServeHTTP(c.Writer, c.Request) })
	r.GET("/test/token", func(c *gin.Context) {
		c.String(http.StatusOK, "time=%d&token=not-implemented", time.Now().Unix()+180)
	})
	r.GET("/test/txUrl", func(c *gin.Context) { respOK(c, gin.H{}) })

	// Mount the Control API (/v3/*) directly on the admin port when
	// available, removing the need for a separate :9997 listener.
	// v3Handler's own routes (RegisterV3Routes) are registered under a
	// "/v3" group, so the incoming path must be forwarded as-is - not
	// rewritten to strip the "/v3" prefix, which would leave v3Handler
	// with no matching route (e.g. "/v3/info" would become "/info", but
	// v3Handler only knows "/v3/info").
	if s.v3Handler != nil {
		r.Any("/v3/*path", func(c *gin.Context) {
			s.v3Handler.ServeHTTP(c.Writer, c.Request)
		})
	}

	// Catch-all → MMX
	r.NoRoute(func(c *gin.Context) {
		// Validate txTime/txSecret only on WHEP (playback), not WHIP (publish)
		if strings.HasSuffix(c.Request.URL.Path, "/whep") {
			if s.TXSecretKeyBack == "" {
				respErr(c, http.StatusServiceUnavailable, "TX_SECRET_KEY_BACK is not configured", "")
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
