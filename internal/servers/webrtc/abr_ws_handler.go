package webrtc

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/bluenviron/mediamtx/internal/logger"
	webrtcproto "github.com/bluenviron/mediamtx/internal/protocols/webrtc"
	"github.com/bluenviron/mediamtx/internal/protocols/websocket"
)

// ABR WS 统一消息格式
type abrMessage struct {
	MsgID     string          `json:"msg_id,omitempty"`
	Type      string          `json:"type"`
	Timestamp int64           `json:"timestamp,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
}

// SELECT_LAYER 请求
type selectLayerData struct {
	TargetTrackID int    `json:"target_track_id"`
	Reason        string `json:"reason"`
}

// LAYER_SWITCHED 响应
type layerSwitchedData struct {
	CurrentTrackID  int `json:"current_track_id"`
	PreviousTrackID int `json:"previous_track_id"`
}

// LATENCY_REPORT 请求
type latencyReportData struct {
	RTTMs           float64 `json:"rtt_ms"`
	JitterBufferMs  float64 `json:"jitter_buffer_ms"`
	PacketsLost     int     `json:"packets_lost"`
	FPS             float64 `json:"fps"`
	EstimatedE2EMs  float64 `json:"estimated_e2e_ms"`
}

// ERROR 响应
type abrErrorData struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// handleABRWebSocket handles the ABR control WebSocket at /ws/control
func (s *httpServer) handleABRWebSocket(ctx *gin.Context) {
	sessionID := ctx.Query("session_id")
	if sessionID == "" {
		s.writeErrorNoLog(ctx, http.StatusBadRequest, fmt.Errorf("missing session_id query parameter"))
		return
	}

	sxUUID, err := uuid.Parse(sessionID)
	if err != nil {
		s.writeErrorNoLog(ctx, http.StatusBadRequest, fmt.Errorf("invalid session_id: %v", err))
		return
	}

	// Find the WHEP session
	sx := s.parent.findSessionByUUID(sxUUID)
	if sx == nil {
		s.writeErrorNoLog(ctx, http.StatusNotFound, fmt.Errorf("session not found"))
		return
	}

	// Verify this is a reader (WHEP) session, not a publisher (WHIP)
	if sx.publish {
		s.writeErrorNoLog(ctx, http.StatusBadRequest, fmt.Errorf("ABR control only available for reader sessions"))
		return
	}

	// Check ABR is enabled and session has a TrackSelector
	sx.mutex.RLock()
	selector := sx.trackSelector
	sx.mutex.RUnlock()

	if selector == nil {
		s.writeErrorNoLog(ctx, http.StatusBadRequest, fmt.Errorf("ABR not available for this session"))
		return
	}

	// Upgrade to WebSocket — unwrap wrapper chain to find the hijackable writer
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
	conn, err := websocket.NewServerConn(hijacker, ctx.Request)
	if err != nil {
		s.Log(logger.Warn, "WebSocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	sx.Log(logger.Info, "ABR WebSocket connected")

	// Bind WS connection to session
	sx.mutex.Lock()
	sx.wsConn = conn
	sx.mutex.Unlock()

	defer func() {
		sx.mutex.Lock()
		sx.wsConn = nil
		sx.mutex.Unlock()
		sx.Log(logger.Info, "ABR WebSocket disconnected")
	}()

	// Send TRACKS_INFO on connect.
	// LAYER_SWITCHED is sent by the callback registered in session.runRead
	// (it reads s.wsConn under s.mutex), so no callback is installed here.
	sendTracksInfo(conn, selector)

	// Message loop
	for {
		var msg abrMessage
		err := conn.ReadJSON(&msg)
		if err != nil {
			sx.Log(logger.Debug, "ABR WS read error: %v", err)
			return
		}

		sx.Log(logger.Debug, "ABR WS received: type=%s", msg.Type)

		switch msg.Type {
		case "SELECT_LAYER":
			var data selectLayerData
			if err := json.Unmarshal(msg.Data, &data); err != nil {
				sendError(conn, 4002, "invalid data format")
				continue
			}

			// Check cooldown
			sx.mutex.RLock()
			lastSwitch := sx.lastSwitchTime
			cooldown := sx.abrSwitchCooldown
			sx.mutex.RUnlock()

			if time.Since(lastSwitch) < time.Duration(cooldown)*time.Millisecond {
				sendError(conn, 4003, "switch too frequent (cooldown period)")
				continue
			}

			err := selector.Select(data.TargetTrackID)
			if err != nil {
				sendError(conn, 4002, err.Error())
				continue
			}

			sx.mutex.Lock()
			sx.lastSwitchTime = time.Now()
			sx.mutex.Unlock()

			sx.Log(logger.Info, "ABR SELECT_LAYER: target=%d reason=%s", data.TargetTrackID, data.Reason)

		case "LATENCY_REPORT":
			var data latencyReportData
			if err := json.Unmarshal(msg.Data, &data); err != nil {
				continue
			}
			// Log latency metrics (can be used for server-side ABR decisions)
			sx.Log(logger.Debug, "ABR LATENCY: rtt=%.1fms jb=%.1fms loss=%d fps=%.1f e2e=%.1fms",
				data.RTTMs, data.JitterBufferMs, data.PacketsLost, data.FPS, data.EstimatedE2EMs)

		case "PING":
			sendPong(conn)

		default:
			sx.Log(logger.Debug, "ABR WS unknown message type: %s", msg.Type)
		}
	}
}

// ── Helper send functions ──────────────────────────────────────

func sendTracksInfo(conn *websocket.ServerConn, selector *webrtcproto.TrackSelector) {
	tracks := selector.GetTracks()
	activeID := selector.ActiveTrackID()

	msg := abrMessage{
		Type: "TRACKS_INFO",
		Data: mustMarshalJSON(map[string]interface{}{
			"active_track_id": activeID,
			"tracks":          tracks,
		}),
	}
	conn.WriteJSON(msg) //nolint:errcheck
}

func sendLayerSwitched(conn *websocket.ServerConn, currentID, previousID int) {
	msg := abrMessage{
		Type: "LAYER_SWITCHED",
		Data: mustMarshalJSON(layerSwitchedData{
			CurrentTrackID:  currentID,
			PreviousTrackID: previousID,
		}),
	}
	conn.WriteJSON(msg) //nolint:errcheck
}

func sendError(conn *websocket.ServerConn, code int, message string) {
	msg := abrMessage{
		Type: "ERROR",
		Data: mustMarshalJSON(abrErrorData{
			Code:    code,
			Message: message,
		}),
	}
	conn.WriteJSON(msg) //nolint:errcheck
}

func sendPong(conn *websocket.ServerConn) {
	msg := abrMessage{
		Type: "PONG",
		Data: mustMarshalJSON(map[string]interface{}{}),
	}
	conn.WriteJSON(msg) //nolint:errcheck
}

func mustMarshalJSON(v interface{}) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("{}")
	}
	return json.RawMessage(data)
}
