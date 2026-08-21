package webrtc

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/bluenviron/mediamtx/internal/logger"
	wsproto "github.com/bluenviron/mediamtx/internal/protocols/websocket"
)

// verifyDegradeAuth checks the connection request for WHIP_WS_SECRET,
// via either an "Authorization: Bearer <secret>" header or a "?token=<secret>"
// query parameter (whichever is easier for the connecting client to set).
//
// This is a static shared secret, not a signed/expiring token like
// split-rec's "advance" mode: unlike that public API, this WS endpoint has
// exactly one intended client (the OBS-side executor, itself under the
// same operator's control), so there's no third party to forge requests
// as and no repeated-request replay surface to worry about - a fixed
// secret is proportionate and far simpler for a native OBS build to
// implement than HMAC-signing a canonical string.
func verifyDegradeAuth(secret string, ctx *gin.Context) bool {
	if secret == "" {
		return false
	}
	token := ctx.Query("token")
	if token == "" {
		if auth := ctx.GetHeader("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			token = strings.TrimPrefix(auth, "Bearer ")
		}
	}
	if token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(secret)) == 1
}

// handleWHIPDegradeWebSocket handles GET /{path}/ws/whip - the control
// channel the OBS-side executor connects to for the degrade protocol (see
// docs/obs-mmx-degrade-protocol.md). The protocol is a one-way,
// declarative push (TARGET_STATE/ALERT); the read loop below exists only
// to detect disconnection and to respond to pings.
func (s *httpServer) handleWHIPDegradeWebSocket(ctx *gin.Context, pathName string) {
	if !s.parent.DegradeEnable {
		s.writeErrorNoLog(ctx, http.StatusNotFound, fmt.Errorf("degrade protocol is disabled"))
		return
	}

	if !verifyDegradeAuth(s.parent.DegradeWSSecret, ctx) {
		// Logged (unlike most writeErrorNoLog call sites): an auth
		// rejection here is the connecting executor's *only* signal that
		// something is wrong (the WS handshake just fails from its
		// perspective, with no detail) - without a line in mmx's own
		// logs, diagnosing it means reading the OBS side's logs instead.
		// Never log the secret or the token supplied.
		s.Log(logger.Warn, "[degrade] path=%s executor connection rejected: invalid or missing token", pathName)
		s.writeErrorNoLog(ctx, http.StatusUnauthorized, fmt.Errorf("publish deviceID authentication failure!"))
		return
	}

	// Unwrap gin's ResponseWriter to find the hijackable one underneath,
	// same as the ABR WebSocket handler.
	var hijacker http.ResponseWriter = ctx.Writer
	for {
		if _, ok := hijacker.(http.Hijacker); ok {
			break
		}
		if uw, ok := hijacker.(interface{ Unwrap() http.ResponseWriter }); ok {
			hijacker = uw.Unwrap()
		} else {
			break
		}
	}

	conn, err := wsproto.NewServerConn(hijacker, ctx.Request)
	if err != nil {
		s.Log(logger.Warn, "degrade WebSocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	ds := s.parent.getOrCreateDegradeState(pathName)
	ds.bindConn(conn)
	defer ds.unbindConn(conn)

	s.Log(logger.Info, "[degrade] path=%s executor connected", pathName)
	defer s.Log(logger.Info, "[degrade] path=%s executor disconnected", pathName)

	// conn.ReadJSON below blocks until the executor disconnects, and the
	// OBS-side executor (WsDegradeClient) deliberately keeps this
	// connection open across WHIP publish/stop cycles (see
	// docs/obs-mmx-degrade-protocol.md) - it is not tied to a WHIP
	// session's lifetime. http.Server.Close() does not close hijacked
	// (upgraded) connections, so without this, a still-connected
	// executor would block this goroutine - and therefore
	// httpp.Server.Close()'s handlerTracker.wg.Wait() - forever on
	// process shutdown. Close conn ourselves once the WebRTC server's
	// context is canceled to unblock ReadJSON and let the handler return.
	shutdownDone := make(chan struct{})
	defer func() { <-shutdownDone }()
	go func() {
		defer close(shutdownDone)
		<-s.parent.ctx.Done()
		conn.Close()
	}()

	for {
		var msg map[string]any
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		// Declarative, server-push protocol: nothing the executor sends
		// is currently acted upon, messages are only read to detect
		// disconnection (and to drain client pongs/keepalives).
	}
}
