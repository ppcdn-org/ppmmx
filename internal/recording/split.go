package recording

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
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

// IngestStarter lets a round-start request bring an ingest-configured path
// online on demand, instead of requiring it to already be publishing (see
// docs/cdn-ingest-thread.md). Satisfied by *internal/ingest.Manager; kept as
// a narrow interface here so this package doesn't need to import it.
type IngestStarter interface {
	StartByPath(path string) (started bool, err error)
	StopByPath(path string)
}

// TableViewResolver looks up every view configured for a table (e.g.
// "table1" -> ["fwh", "fwv"]), so a round-start/round-end request - which
// only ever carries the table name, not a specific view - can be applied
// to every configured view at once instead of guessing a single default.
// The caller (tableToPaths) combines each view with the request's appId to
// derive the actual stream path (appId+"/"+table+"-"+view - see
// docs/design/ppcdn-mmx-publish-whitelist.zh-CN.md): the path always lives
// under the requesting app's own namespace, so this resolver only needs to
// know which views a table has, not where any app's streams live.
// Satisfied by *internal/admin.Store.
type TableViewResolver interface {
	ViewsForTable(table string) ([]string, error)
}

// TableViewPath is one view's resolved stream path for a table, as
// returned by TableViewResolver.
type TableViewPath struct {
	Path string
	View string
}

// waitForIngestPath bounds how long a round-start request blocks for an
// ingest-triggered path to actually come online (ffmpeg dialing the source,
// mediamtx accepting the RTMP publish) before giving up.
const waitForIngestPath = 10 * time.Second

// split-rec's `time` field is the signing instant (docs/design/
// ppcdn-external-api.md §5.2/§5.6: callers compute it as T=$(date +%s) at
// request time), not an expiry deadline - a request is accepted only while
// `time` is within one of these windows of the server's clock, in either
// direction. Simple mode's window is intentionally tight since a captured
// request can be replayed verbatim any time inside it; advance mode's is
// wider (matching §5.3's "time 不能超过当前时间 5 分钟以后") because its
// nonce (see useNonce) is what actually prevents replay, not the window.
const (
	simpleModeTimeTolerance  = 30 * time.Second
	advanceModeTimeTolerance = 5 * time.Minute
)

// AppSecretLookup resolves an appId's own appSecret, the credential
// split-rec signatures are now verified against (see verifyToken) instead
// of a single deployment-wide shared secret. Satisfied by
// *internal/core.appPublishWhitelist, which syncs appId+appSecret pairs
// from ppcenter alongside the publish whitelist itself (see
// docs/design/ppcdn-mmx-publish-whitelist.zh-CN.md).
type AppSecretLookup interface {
	AppSecret(appID string) (string, bool)
}

// SplitRecHandler handles POST /api/record/split requests.
type SplitRecHandler struct {
	mgr                  *Manager
	pathFinder           PathFinder
	parent               logger.Writer
	mu                   sync.Mutex
	rateLim              map[string]*rateLimitEntry // per-IP rate limiting
	authMode             string
	appSecrets           AppSecretLookup
	nonces               map[string]time.Time
	uploader             *uploader
	splitRecFileReporter SplitRecFileReporter
	ingestMgr            IngestStarter
	viewResolver         TableViewResolver
	// activeGames tracks, per table, which owner (see ownerKey) currently
	// holds the open recording round (started by a gameRound-less call,
	// closed by the paired call carrying gameRound). Only one owner may
	// hold a table at a time - not one owner per view/path, since a
	// round-start/round-end request only ever carries the table name.
	activeGames map[string]activeRound
}

// activeRound identifies the owner holding a round open (see ownerKey), the
// path+view pairs that were actually started for it (a table with N
// configured views starts N recordings), and the per-path audit record (in
// mgr.Store()) tracking each, if any.
type activeRound struct {
	owner     string // ownerKey(req): appEnv+":"+gameId, or just gameId if appEnv is absent
	game      string // original gameId field, kept for messages/audit independent of appEnv
	startedAt time.Time
	paths     []TableViewPath
	recordIDs map[string]string // path -> recordID
}

type rateLimitEntry struct {
	count     int
	resetTime time.Time
}

// identifierPattern restricts tableId/gameId/gameRound to characters that
// are safe to embed in a file name (no $, *, /, spaces, etc.).
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

// SetIngestManager wires in the ingest manager that lets round-start
// requests bring an ingest-configured path online on demand. Not calling
// this (ingestMgr stays nil) just means round-start requests behave as
// before: the path must already be publishing.
func (h *SplitRecHandler) SetIngestManager(m IngestStarter) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ingestMgr = m
}

// SetViewResolver wires in the source of truth for table->views, letting
// round-start/round-end requests fan out to every view configured for a
// table. Not calling this (viewResolver stays nil) falls back to the
// legacy tablePathMapping/single-default behavior.
func (h *SplitRecHandler) SetViewResolver(r TableViewResolver) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.viewResolver = r
}

// ConfigureUpload changes the net-storage upload settings without
// recreating the handler. Called again on every config reload; carries the
// already-wired splitRecFileReporter forward onto the new uploader instance
// (see SetSplitRecFileReporter), since reload order doesn't guarantee that
// method runs again after this one.
func (h *SplitRecHandler) ConfigureUpload(cfg UploadConfig) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.uploader = newUploader(cfg, h.parent)
	h.uploader.reporter = h.splitRecFileReporter
}

// SetSplitRecFileReporter wires in the client used to tell ppcenter about a
// round file once its upload succeeds (see SplitRecFileReporter). Not
// calling this (splitRecFileReporter stays nil) just means round files are
// never reported - uploading itself is unaffected either way. Safe to call
// before or after ConfigureUpload.
func (h *SplitRecHandler) SetSplitRecFileReporter(r SplitRecFileReporter) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.splitRecFileReporter = r
	if h.uploader != nil {
		h.uploader.reporter = r
	}
}

// ConfigureAuth changes split-rec's signing scheme ("simple" md5 or
// "advance" HMAC-SHA256+nonce) without recreating the handler. The secret
// itself is no longer a deployment-wide config value - see
// SetAppSecretLookup.
func (h *SplitRecHandler) ConfigureAuth(mode string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.authMode = mode
	h.nonces = make(map[string]time.Time)
}

// SetAppSecretLookup wires in the source of truth for appId->appSecret,
// letting verifyToken check a request's signature against the calling
// app's own credential (see AppSecretLookup's doc comment). Not calling
// this (appSecrets stays nil) makes every request fail closed in
// verifyToken, the same fail-safe direction as an unset deployment secret
// used to.
func (h *SplitRecHandler) SetAppSecretLookup(l AppSecretLookup) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.appSecrets = l
}

type splitRecRequest struct {
	Time      string `json:"time"`
	AppID     string `json:"appId"`
	TableID   string `json:"tableId"`
	GameRound string `json:"gameRound"`
	GameID    string `json:"gameId"`
	AppEnv    string `json:"appEnv"`
}

// ownerKey identifies who holds a round open (see activeGames): appEnv
// combined with gameId if appEnv was provided, otherwise just gameId. Two
// round-start calls with the same gameId but different appEnv are treated
// as different owners - e.g. appEnv "test"+gameId "p2w001" and appEnv
// "prod"+gameId "p2w001" can each hold their own round independently, but a
// round can only ever be stopped by the same owner that started it.
func ownerKey(req splitRecRequest) string {
	if req.AppEnv != "" {
		return req.AppEnv + ":" + req.GameID
	}
	return req.GameID
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
			"path": "/api/record/split",
		}))
		return
	}

	var req splitRecRequest
	if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
		c.JSON(http.StatusBadRequest, errResp(400, "invalid request: "+err.Error(), ""))
		return
	}

	// Validate required fields. gameId is mandatory in both directions: it
	// identifies who owns the round (see execute/activeGames). appId is
	// mandatory too - it's the only input that decides which app's
	// publish namespace this round-start/round-end resolves into (see
	// tableToPaths), so an omitted appId can never silently fall back to
	// some other app's stream.
	auth := c.GetHeader("Authorization")
	if req.Time == "" || req.AppID == "" || req.TableID == "" || req.GameID == "" || auth == "" {
		c.JSON(http.StatusBadRequest, errResp(400, "missing parameters.", ""))
		return
	}
	if !validIdentifier(req.AppID) || !validIdentifier(req.TableID) || !validIdentifier(req.GameID) ||
		(req.GameRound != "" && !validIdentifier(req.GameRound)) ||
		(req.AppEnv != "" && !validIdentifier(req.AppEnv)) {
		c.JSON(http.StatusBadRequest, errResp(400, "appId/tableId/gameId/gameRound/appEnv contain invalid characters.", ""))
		return
	}

	// Verify token
	if !h.verifyToken(auth, c.GetHeader("X-Split-Rec-Nonce"), req) {
		c.JSON(http.StatusUnauthorized, errResp(401, "invalid token!", ""))
		return
	}

	// Check freshness: req.Time is the signing instant, accepted within a
	// tolerance window of the server's clock (see the doc comment on
	// simpleModeTimeTolerance/advanceModeTimeTolerance).
	signingInstant, err := parseUnixTime(req.Time)
	if err != nil {
		c.JSON(http.StatusUnauthorized, errResp(401, "token expired!", ""))
		return
	}
	tolerance := simpleModeTimeTolerance
	isAdvance := h.authModeValue() == "advance"
	if isAdvance {
		tolerance = advanceModeTimeTolerance
	}
	if drift := time.Since(signingInstant); drift > tolerance || drift < -tolerance {
		c.JSON(http.StatusUnauthorized, errResp(401, "token expired!", ""))
		return
	}
	if isAdvance {
		// A nonce only needs to be remembered until it would fail the
		// freshness check on its own anyway - retaining it past that point
		// just grows h.nonces for no benefit.
		if !h.useNonce(c.GetHeader("X-Split-Rec-Nonce"), signingInstant.Add(advanceModeTimeTolerance)) {
			c.JSON(http.StatusUnauthorized, errResp(401, "nonce already used!", ""))
			return
		}
	}

	// Execute business logic. errStreamOffline gets its own code (still
	// under HTTP 500, so existing clients that only branch on HTTP status
	// keep working) so a caller can tell "this app owns the table but
	// isn't streaming right now" apart from every other 500 (locking
	// conflicts, split failures, etc.) without string-matching msg.
	if err := h.execute(c, req); err != nil {
		if _, ok := err.(errStreamOffline); ok {
			c.JSON(http.StatusInternalServerError, errResp(50001, err.Error(), ""))
			return
		}
		c.JSON(http.StatusInternalServerError, errResp(500, err.Error(), ""))
		return
	}

	c.JSON(http.StatusOK, okResp())
}

func (h *SplitRecHandler) verifyToken(token, nonce string, req splitRecRequest) bool {
	h.mu.Lock()
	mode := h.authMode
	lookup := h.appSecrets
	h.mu.Unlock()

	if lookup == nil {
		// No appSecret source wired in at all: fail closed, same direction
		// as an unset deployment secret used to be.
		return false
	}
	appSecret, ok := lookup.AppSecret(req.AppID)
	if !ok || appSecret == "" {
		// Unknown/ineligible appId (never synced, deleted, arrears) or an
		// app row with no secret: fail closed rather than falling through
		// to an unkeyed comparison, either of which an outside caller
		// could otherwise compute without knowing anything secret at all.
		return false
	}
	secret := []byte(appSecret)

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
		// AppID is included in the signed canonical JSON (not just
		// validated for character set) - it decides which app's publish
		// namespace a round resolves into (see tableToPaths), so leaving
		// it unsigned would let a captured/valid signature for one appId
		// be replayed with a different appId substituted in.
		canonical, _ := json.Marshal(struct {
			Time      string `json:"time"`
			AppID     string `json:"appId"`
			TableID   string `json:"tableId"`
			GameRound string `json:"gameRound"`
			GameID    string `json:"gameId"`
			Nonce     string `json:"nonce"`
		}{req.Time, req.AppID, req.TableID, req.GameRound, req.GameID, nonce})
		mac := hmac.New(sha256.New, secret)
		mac.Write(canonical)
		return hmac.Equal(signature, mac.Sum(nil))
	}

	// AppID participates in the base string for the same reason it's in
	// advance mode's canonical JSON above: it must be tamper-protected by
	// the signature, not just character-set validated.
	base := string(secret) + req.Time + req.AppID + req.TableID
	if req.GameRound != "" {
		base += req.GameRound
	}
	if req.GameID != "" {
		base += req.GameID
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

// tableLockKey scopes the activeGames lock to (appId, tableId) rather than
// tableId alone: a table path is only unique once its owning app is known
// (see tableToPaths - appA/table-view and appB/table-view are unrelated
// physical streams), so two different apps using the same tableId string
// must never contend for the same lock entry.
func tableLockKey(appID, table string) string {
	return appID + "/" + table
}

// execute implements the two-phase per-round protocol:
//   - gameRound empty: round start. For every path matching (req.AppID,
//     req.TableID) (a table with multiple configured views records all of
//     them - see tableToPaths), cuts a fresh segment on the path's already
//     running recording (record: yes) and locks the whole table to the
//     caller's owner identity (see ownerKey: appEnv+gameId if appEnv was
//     provided, otherwise just gameId). If a given path has nothing
//     publishing to it yet and an ingest source is configured for it,
//     this is also what brings the ingest pull online (see
//     resolveController) - ingest only ever runs because a round started,
//     not from process boot. A path with no resolvable controller at all
//     is skipped with a warning rather than failing the whole round,
//     unless every path fails, in which case the round-start itself
//     fails.
//   - gameRound non-empty: round end. Cuts the segment that has been
//     accumulating on every path started for this round since the paired
//     start call, renames each to "$tableId-$view-$gameRound-$gameId",
//     releases the table lock, and - if ingest was what brought a path
//     online - stops the pull again for it.
//
// A (appId, tableId) pair can only have one owner holding it open at a
// time: a second start is rejected, and only the same owner
// (appEnv+gameId, or gameId alone when appEnv is omitted) that opened the
// round may close it - a different appEnv with the same gameId is a
// different owner and cannot stop that round.
func (h *SplitRecHandler) execute(c *gin.Context, req splitRecRequest) error {
	if req.GameRound == "" {
		return h.startRound(c.Request.Context(), req)
	}
	return h.stopRound(req)
}

// resolveController looks up path's controller, and - only for a
// round-start call (allowStartIngest) with no publisher already live -
// falls back to asking the ingest manager to start pulling it, then polls
// every pollInterval, up to timeout, for it to actually come online. A
// round-end call never triggers ingest: there's nothing to wait for if a
// round was never started.
func (h *SplitRecHandler) resolveController(
	ctx context.Context, path string, allowStartIngest bool, timeout, pollInterval time.Duration,
) (PathController, bool) {
	if ctrl, ok := h.lookupController(path); ok {
		return ctrl, true
	}

	if !allowStartIngest {
		return nil, false
	}

	h.mu.Lock()
	ingestMgr := h.ingestMgr
	h.mu.Unlock()
	if ingestMgr == nil {
		return nil, false
	}

	if _, err := ingestMgr.StartByPath(path); err != nil {
		// Nothing configured for this path at all - waiting would be
		// pointless, and the caller reports "stream not found" either way.
		return nil, false
	}

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		if ctrl, ok := h.lookupController(path); ok {
			return ctrl, true
		}
		select {
		case <-ticker.C:
			if time.Now().After(deadline) {
				return nil, false
			}
		case <-ctx.Done():
			return nil, false
		}
	}
}

// lookupController tries the handler's own recording-job map first, then
// falls back to the general path manager.
func (h *SplitRecHandler) lookupController(path string) (PathController, bool) {
	if ctrl, ok := h.findController(path); ok {
		return ctrl, true
	}
	if h.pathFinder != nil {
		return h.pathFinder.FindPath(path)
	}
	return nil, false
}

// startRound opens a round for every path configured for (req.AppID,
// req.TableID) (see tableToPaths). It locks the table to the caller's
// owner identity first (see ownerKey), so a concurrent start for the same
// (appId, tableId) is rejected before any path is touched; if every path
// then fails to resolve or split, the table lock is released and the
// round-start fails. A path that individually fails (no publisher, ingest
// never came online, split error) is logged and skipped rather than
// aborting paths that did succeed - the recording continues on whichever
// views are actually live.
func (h *SplitRecHandler) startRound(ctx context.Context, req splitRecRequest) error {
	owner := ownerKey(req)
	lockKey := tableLockKey(req.AppID, req.TableID)
	h.mu.Lock()
	if round, active := h.activeGames[lockKey]; active {
		h.mu.Unlock()
		if round.owner == owner {
			return fmt.Errorf("game %q already has an active recording on table %q", req.GameID, req.TableID)
		}
		return fmt.Errorf("table %q is already being recorded by another game", req.TableID)
	}
	h.activeGames[lockKey] = activeRound{owner: owner, game: req.GameID, startedAt: now()}
	h.mu.Unlock()

	startedAt := now()
	paths := h.tableToPaths(req.AppID, req.TableID)
	recordIDs := make(map[string]string, len(paths))
	var startedPaths []TableViewPath
	for _, tvp := range paths {
		ctrl, ok := h.resolveController(ctx, tvp.Path, true, waitForIngestPath, 200*time.Millisecond)
		if !ok {
			h.parent.Log(logger.Warn, "[split-rec] table %q: path %q not found, skipping", req.TableID, tvp.Path)
			continue
		}
		if _, err := ctrl.SplitRecording(""); err != nil {
			h.parent.Log(logger.Warn, "[split-rec] table %q: start round on path %q failed: %v", req.TableID, tvp.Path, err)
			continue
		}
		recordID := newRecordID(tvp.Path)
		recordIDs[tvp.Path] = recordID
		startedPaths = append(startedPaths, tvp)

		if h.mgr != nil {
			_ = h.mgr.Store().Insert(Record{
				ID:        recordID,
				Path:      tvp.Path,
				Table:     req.TableID,
				Game:      req.GameID,
				AppEnv:    req.AppEnv,
				Format:    "fmp4",
				Status:    "running",
				StartedAt: startedAt,
			})
		}
	}

	if len(startedPaths) == 0 {
		h.mu.Lock()
		delete(h.activeGames, lockKey)
		h.mu.Unlock()
		// Every path is deterministically derived from (appId, tableId,
		// view) - see tableToPaths - so there's no "path not configured"
		// failure mode left to distinguish; not finding a live controller
		// on any of them only ever means the app isn't currently
		// streaming to this table.
		return errStreamOffline{appID: req.AppID, table: req.TableID}
	}

	h.mu.Lock()
	h.activeGames[lockKey] = activeRound{
		owner:     owner,
		game:      req.GameID,
		startedAt: startedAt,
		paths:     startedPaths,
		recordIDs: recordIDs,
	}
	h.mu.Unlock()
	return nil
}

// errStreamOffline is returned by startRound when every configured view's
// stream path exists (resolveController found a matching path shape to
// even check) but none of them are currently online - distinguishing "the
// app owns this table but isn't streaming right now" from "start round: no
// path found", which covers configuration/routing problems instead. See
// ServeHTTP's error-code mapping.
type errStreamOffline struct {
	appID string
	table string
}

func (e errStreamOffline) Error() string {
	return fmt.Sprintf("stream for app %q table %q is not online", e.appID, e.table)
}

// stopRound closes the round for every path that was actually started for
// this table (see startRound), renaming each finished segment and
// completing its audit record. A per-path failure is logged and skipped,
// same as startRound, so one broken view doesn't prevent finalizing the
// others.
//
// A stop call for a table with no active round at all (no matching
// start was ever received - as opposed to one held by a different
// owner, which is still rejected below) is treated as a no-op rather
// than an error: it's silently dropped with just a log line, returning
// success. This avoids surfacing 500s/alerts for a caller-side stop
// signal that fires unconditionally regardless of whether a start
// actually preceded it.
func (h *SplitRecHandler) stopRound(req splitRecRequest) error {
	lockKey := tableLockKey(req.AppID, req.TableID)
	h.mu.Lock()
	round, active := h.activeGames[lockKey]
	if !active {
		h.mu.Unlock()
		h.parent.Log(logger.Warn,
			"[split-rec] table %q: stop round dropped, no active recording (gameId=%q gameRound=%q)",
			req.TableID, req.GameID, req.GameRound)
		return nil
	}
	if round.owner != ownerKey(req) {
		h.mu.Unlock()
		return fmt.Errorf("recording on table %q was started by a different game", req.TableID)
	}
	delete(h.activeGames, lockKey)
	h.mu.Unlock()

	h.mu.Lock()
	ingestMgr := h.ingestMgr
	up := h.uploader
	h.mu.Unlock()

	var stopErrs []string
	for _, tvp := range round.paths {
		path := tvp.Path
		ctrl, ok := h.lookupController(path)
		if !ok {
			stopErrs = append(stopErrs, fmt.Sprintf("path %q not found", path))
			h.parent.Log(logger.Warn, "[split-rec] table %q: stop round on path %q failed: not found", req.TableID, path)
			continue
		}

		finalName := req.TableID + "-" + tvp.View + "-" + req.GameRound + "-" + req.GameID
		finalPath, err := ctrl.SplitRecording(finalName)
		if err != nil {
			stopErrs = append(stopErrs, fmt.Sprintf("path %q: %v", path, err))
			h.parent.Log(logger.Warn, "[split-rec] table %q: stop round on path %q failed: %v", req.TableID, path, err)
			// Symmetric with the on-demand start: stopping the round is
			// what ends an ingest-sourced pull, whether or not the split
			// itself succeeded.
			if ingestMgr != nil {
				ingestMgr.StopByPath(path)
			}
			continue
		}

		var fileSize int64
		if statInfo, statErr := os.Stat(finalPath); statErr == nil {
			fileSize = statInfo.Size()
		}
		duration := int64(time.Since(round.startedAt).Seconds())

		if h.mgr != nil {
			if recordID, ok := round.recordIDs[path]; ok {
				_ = h.mgr.Store().CompleteRound(recordID, req.GameRound, finalPath, fileSize, duration)
			}
		}

		up.uploadAsync(finalPath, objectKeyFor(finalPath), req.AppEnv, SplitRecFileInfo{
			TableID:         req.TableID,
			GameID:          req.GameID,
			GameRound:       req.GameRound,
			AppEnv:          req.AppEnv,
			StreamPath:      path,
			FileName:        filepath.Base(finalPath),
			DurationSeconds: duration,
			SizeBytes:       fileSize,
		})

		if ingestMgr != nil {
			ingestMgr.StopByPath(path)
		}
	}

	if len(stopErrs) > 0 && len(stopErrs) == len(round.paths) {
		return fmt.Errorf("stop round: %s", strings.Join(stopErrs, "; "))
	}
	return nil
}

// newRecordID generates a unique id for a split-rec audit record.
func newRecordID(path string) string {
	return fmt.Sprintf("splitrec_%x", md5.Sum([]byte(uuid.NewString()+path)))
}

// tableToPaths resolves every path+view a round-start/round-end request for
// (appID, table) must be applied to. If a TableViewResolver is wired in
// and reports at least one view for table, one TableViewPath is returned
// per configured view - a table with multiple views records all of them,
// since the request only carries the table name, not a specific view.
// Otherwise falls back to a single view, "fwh" (matching the convention
// every fallback in this file already assumed before views were
// configurable). Every path is derived the same way regardless of source:
// appID+"/"+table+"-"+view, so it always resolves under the requesting
// app's own WHIP-publish namespace (see docs/design/
// ppcdn-mmx-publish-whitelist.zh-CN.md) rather than a separately
// registered path.
func (h *SplitRecHandler) tableToPaths(appID, table string) []TableViewPath {
	h.mu.Lock()
	resolver := h.viewResolver
	h.mu.Unlock()

	views := []string{"fwh"}
	if resolver != nil {
		if resolved, err := resolver.ViewsForTable(table); err == nil && len(resolved) > 0 {
			views = resolved
		}
	}

	paths := make([]TableViewPath, len(views))
	for i, view := range views {
		paths[i] = TableViewPath{Path: appID + "/" + table + "-" + view, View: view}
	}
	return paths
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
