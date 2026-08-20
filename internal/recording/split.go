package recording

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/bluenviron/mediamtx/internal/logger"
)

// PathFinder is the interface for looking up stream paths for recording.
type PathFinder interface {
	FindPath(name string) (PathController, bool)
}

// SplitRecHandler handles POST /api/split-rec requests.
type SplitRecHandler struct {
	mgr        *Manager
	pathFinder PathFinder
	parent     logger.Writer
	mu         sync.Mutex
	rateLim    map[string]*rateLimitEntry // per-IP rate limiting
	authMode   string
	authSecret []byte
	nonces     map[string]time.Time
	uploader   *uploader
	// activeGames tracks, per resolved path, which game currently owns the
	// open recording round (started by a gc-less call, closed by the paired
	// call carrying gc). Only one game may hold a path at a time.
	activeGames map[string]activeRound
}

// activeRound identifies the game holding a round open and the audit
// record (in mgr.Store()) tracking it, if any.
type activeRound struct {
	game      string
	recordID  string
	startedAt time.Time
}

type rateLimitEntry struct {
	count     int
	resetTime time.Time
}

// identifierPattern restricts table/game/gc to characters that are safe to
// embed in a file name (no $, *, /, spaces, etc.).
var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func validIdentifier(s string) bool {
	return identifierPattern.MatchString(s)
}

// NewSplitRecHandler creates a SplitRecHandler.
func NewSplitRecHandler(mgr *Manager, pf PathFinder, parent logger.Writer) *SplitRecHandler {
	return &SplitRecHandler{
		mgr:         mgr,
		pathFinder:  pf,
		parent:      parent,
		rateLim:     make(map[string]*rateLimitEntry),
		authMode:    "simple",
		nonces:      make(map[string]time.Time),
		activeGames: make(map[string]activeRound),
	}
}

// ConfigureUpload changes the net-storage upload settings without
// recreating the handler. Called again on every config reload.
func (h *SplitRecHandler) ConfigureUpload(cfg UploadConfig) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.uploader = newUploader(cfg, h.parent)
}

// ConfigureAuth changes split-rec authentication without recreating the handler.
func (h *SplitRecHandler) ConfigureAuth(mode, secret string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.authMode = mode
	h.authSecret = []byte(secret)
	h.nonces = make(map[string]time.Time)
}

type splitRecRequest struct {
	Time  string `json:"time"`
	Table string `json:"table"`
	GC    string `json:"gc"`
	Game  string `json:"game"`
}

type splitRecResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data any    `json:"data"`
}

func okResp() splitRecResponse { return splitRecResponse{Code: 200, Msg: "OK"} }
func errResp(code int, msg string, data any) splitRecResponse {
	return splitRecResponse{Code: code, Msg: msg, Data: data}
}

func (h *SplitRecHandler) ServeHTTP(c *gin.Context) {
	// Rate limiting: 500 QPS per IP per path
	ip := c.ClientIP()
	if !h.checkRateLimit(ip) {
		c.JSON(http.StatusTooManyRequests, errResp(429, "rate limit exceeded: max 500 req/s per IP per API", map[string]string{
			"ip":   ip,
			"path": "/api/split-rec",
		}))
		return
	}

	var req splitRecRequest
	if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
		c.JSON(http.StatusBadRequest, errResp(400, "invalid request: "+err.Error(), ""))
		return
	}

	// Validate required fields. game is mandatory in both directions: it
	// identifies who owns the round (see execute/activeGames).
	auth := c.GetHeader("Authorization")
	if req.Time == "" || req.Table == "" || req.Game == "" || auth == "" {
		c.JSON(http.StatusBadRequest, errResp(400, "missing parameters.", ""))
		return
	}
	if !validIdentifier(req.Table) || !validIdentifier(req.Game) || (req.GC != "" && !validIdentifier(req.GC)) {
		c.JSON(http.StatusBadRequest, errResp(400, "table/game/gc contain invalid characters.", ""))
		return
	}

	// Verify token
	if !h.verifyToken(auth, c.GetHeader("X-Split-Rec-Nonce"), req) {
		c.JSON(http.StatusUnauthorized, errResp(401, "invalid token!", ""))
		return
	}

	// Check expiry
	expiry, err := parseUnixTime(req.Time)
	if err != nil || time.Now().After(expiry) {
		c.JSON(http.StatusUnauthorized, errResp(401, "token expired!", ""))
		return
	}
	if h.authModeValue() == "advance" {
		if expiry.After(time.Now().Add(5 * time.Minute)) {
			c.JSON(http.StatusUnauthorized, errResp(401, "token expiry exceeds 5 minutes!", ""))
			return
		}
		if !h.useNonce(c.GetHeader("X-Split-Rec-Nonce"), expiry) {
			c.JSON(http.StatusUnauthorized, errResp(401, "nonce already used!", ""))
			return
		}
	}

	// Execute business logic
	if err := h.execute(c, req); err != nil {
		c.JSON(http.StatusInternalServerError, errResp(500, err.Error(), ""))
		return
	}

	c.JSON(http.StatusOK, okResp())
}

func (h *SplitRecHandler) verifyToken(token, nonce string, req splitRecRequest) bool {
	h.mu.Lock()
	mode := h.authMode
	secret := append([]byte(nil), h.authSecret...)
	h.mu.Unlock()

	if mode == "advance" {
		if len(nonce) < 16 || len(nonce) > 128 || strings.ContainsAny(nonce, "\r\n") {
			return false
		}
		const prefix = "HMAC-SHA256 "
		if !strings.HasPrefix(token, prefix) {
			return false
		}
		signature, err := hex.DecodeString(strings.TrimPrefix(token, prefix))
		if err != nil || len(signature) != sha256.Size {
			return false
		}
		canonical, _ := json.Marshal(struct {
			Time  string `json:"time"`
			Table string `json:"table"`
			GC    string `json:"gc"`
			Game  string `json:"game"`
			Nonce string `json:"nonce"`
		}{req.Time, req.Table, req.GC, req.Game, nonce})
		mac := hmac.New(sha256.New, secret)
		mac.Write(canonical)
		return hmac.Equal(signature, mac.Sum(nil))
	}

	base := "lotto-dvr" + req.Time + req.Table
	if req.GC != "" {
		base += req.GC
	}
	if req.Game != "" {
		base += req.Game
	}
	expected := fmt.Sprintf("%x", md5.Sum([]byte(base)))
	return strings.EqualFold(token, expected)
}

func (h *SplitRecHandler) authModeValue() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.authMode
}

func (h *SplitRecHandler) useNonce(nonce string, expiry time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	for key, expiresAt := range h.nonces {
		if now.After(expiresAt) {
			delete(h.nonces, key)
		}
	}
	if _, ok := h.nonces[nonce]; ok {
		return false
	}
	h.nonces[nonce] = expiry
	return true
}

// execute implements the two-phase per-round protocol:
//   - gc empty: round start. Cuts a fresh segment on the path's already
//     running recording (record: yes) and locks the path to req.Game.
//   - gc non-empty: round end. Cuts the segment that has been accumulating
//     since the paired start call, renames it to
//     "$table-$view-$gc-$game", and releases the lock.
//
// A path can only have one game holding it open at a time: a second start
// is rejected, and only the game that opened the round may close it.
func (h *SplitRecHandler) execute(c *gin.Context, req splitRecRequest) error {
	path := tableToPath(req.Table)

	ctrl, ok := h.findController(path)
	if !ok && h.pathFinder != nil {
		ctrl, ok = h.pathFinder.FindPath(path)
	}
	if !ok {
		return fmt.Errorf("stream %q not found", path)
	}

	if req.GC == "" {
		return h.startRound(path, req, ctrl)
	}
	return h.stopRound(path, req, ctrl)
}

func (h *SplitRecHandler) startRound(path string, req splitRecRequest, ctrl PathController) error {
	h.mu.Lock()
	if round, active := h.activeGames[path]; active {
		h.mu.Unlock()
		if round.game == req.Game {
			return fmt.Errorf("game %q already has an active recording on table %q", req.Game, req.Table)
		}
		return fmt.Errorf("table %q is already being recorded by another game", req.Table)
	}
	round := activeRound{game: req.Game, recordID: newRecordID(path), startedAt: time.Now()}
	h.activeGames[path] = round
	h.mu.Unlock()

	if _, err := ctrl.SplitRecording(""); err != nil {
		h.mu.Lock()
		delete(h.activeGames, path)
		h.mu.Unlock()
		return fmt.Errorf("start round: %w", err)
	}

	if h.mgr != nil {
		_ = h.mgr.Store().Insert(Record{
			ID:        round.recordID,
			Path:      path,
			Table:     req.Table,
			Game:      req.Game,
			Format:    "fmp4",
			Status:    "running",
			StartedAt: round.startedAt,
		})
	}
	return nil
}

func (h *SplitRecHandler) stopRound(path string, req splitRecRequest, ctrl PathController) error {
	h.mu.Lock()
	round, active := h.activeGames[path]
	if !active {
		h.mu.Unlock()
		return fmt.Errorf("no active recording on table %q", req.Table)
	}
	if round.game != req.Game {
		h.mu.Unlock()
		return fmt.Errorf("recording on table %q was started by a different game", req.Table)
	}
	delete(h.activeGames, path)
	h.mu.Unlock()

	finalName := req.Table + "-" + viewFromPath(req.Table, path) + "-" + req.GC + "-" + req.Game
	finalPath, err := ctrl.SplitRecording(finalName)
	if err != nil {
		return fmt.Errorf("stop round: %w", err)
	}

	if h.mgr != nil {
		var fileSize int64
		if info, statErr := os.Stat(finalPath); statErr == nil {
			fileSize = info.Size()
		}
		duration := int64(time.Since(round.startedAt).Seconds())
		_ = h.mgr.Store().CompleteRound(round.recordID, req.GC, finalPath, fileSize, duration)
	}

	h.mu.Lock()
	up := h.uploader
	h.mu.Unlock()
	up.uploadAsync(finalPath, objectKeyFor(finalPath))

	return nil
}

// newRecordID generates a unique id for a split-rec audit record.
func newRecordID(path string) string {
	return fmt.Sprintf("splitrec_%x", md5.Sum([]byte(uuid.NewString()+path)))
}

// viewFromPath extracts the view id from a resolved path, e.g. table
// "3drush" and path "live/3drush-fwv" yield "fwv".
func viewFromPath(table, path string) string {
	segment := path
	if idx := strings.LastIndexByte(path, '/'); idx >= 0 {
		segment = path[idx+1:]
	}
	return strings.TrimPrefix(segment, table+"-")
}

// Table-to-path mapping. Override by registering externally.
var tablePathMapping = map[string]string{}

// SetTablePathMapping sets the table→path mapping for split-rec.
func SetTablePathMapping(m map[string]string) {
	tablePathMapping = m
}

func tableToPath(table string) string {
	if p, ok := tablePathMapping[table]; ok {
		return p
	}
	return "live/" + table + "-fwh"
}

func (h *SplitRecHandler) findController(path string) (PathController, bool) {
	h.mgr.mu.RLock()
	defer h.mgr.mu.RUnlock()

	job, ok := h.mgr.running[path]
	if ok {
		return job.ctrl, true
	}
	return nil, false
}

func (h *SplitRecHandler) checkRateLimit(ip string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	now := time.Now()
	e, ok := h.rateLim[ip]
	if !ok || now.After(e.resetTime) {
		h.rateLim[ip] = &rateLimitEntry{count: 1, resetTime: now.Add(time.Second)}
		return true
	}
	e.count++
	return e.count <= 500
}

func parseUnixTime(s string) (time.Time, error) {
	var sec int64
	if _, err := fmt.Sscanf(s, "%d", &sec); err != nil {
		return time.Time{}, err
	}
	return time.Unix(sec, 0), nil
}
