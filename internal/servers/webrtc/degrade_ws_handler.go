package webrtc

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/protocols/publishtoken"
	wsproto "github.com/bluenviron/mediamtx/internal/protocols/websocket"
)

// verifyDegradeAuth checks that the connection request carries a
// ppcenter-issued publish bearer token valid for this exact path, via either
// an "Authorization: Bearer <token>" header or a "?token=<token>" query
// parameter. It reuses the identical credential and key as WHIP publish
// (see checkWHIPDeviceID and docs/obs-whip-publish-auth-protocol.md): the
// OBS-side executor sends the whipTracks bearer_token it already obtained
// from ppcenter for this codec, so there is no separate shared secret to
// provision or keep in sync any more.
//
// The token is codec-bound (appId/stream/codec), and pathName here is
// exactly that 3-segment path (the degrade WS URL is derived from the WHIP
// URL by swapping the trailing "/whip" for "/ws/whip", so the codec segment
// is preserved) - claims.PathMatches enforces the binding.
func verifyDegradeAuth(authKey, pathName string, ctx *gin.Context) bool {
	if authKey == "" {
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
	claims, err := publishtoken.Decrypt(authKey, token, time.Now())
	if err != nil {
		return false
	}
	return claims.PathMatches(pathName)
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

	if !verifyDegradeAuth(s.parent.WHIPAuthKey, pathName, ctx) {
		// Logged (unlike most writeErrorNoLog call sites): an auth
		// rejection here is the connecting executor's *only* signal that
		// something is wrong (the WS handshake just fails from its
		// perspective, with no detail) - without a line in mmx's own
		// logs, diagnosing it means reading the OBS side's logs instead.
		// Never log the token supplied.
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

	ds := s.parent.DegradeManager.GetOrCreate(pathName)
	ds.BindConn(conn)
	defer ds.UnbindConn(conn)

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
