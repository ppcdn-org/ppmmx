package mmxcontrol

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/bluenviron/mediamtx/internal/logger"
)

type testLogger struct{}

func (testLogger) Log(logger.Level, string, ...any) {}

func TestClientSendsRegistrationThenHeartbeat(t *testing.T) {
	messages := make(chan []byte, 4)
	server := newControlServer(t, func(conn *websocket.Conn) {
		_, registration, err := conn.ReadMessage()
		if err != nil {
			return
		}
		messages <- registration
		require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, registerAck(true, "")))
		_, heartbeat, err := conn.ReadMessage()
		if err == nil {
			messages <- heartbeat
		}
	})
	defer server.Close()

	var mu sync.Mutex
	pipelines := []string{"app/b", "app/a", "app/a"}
	client := New(context.Background(), Config{
		URL: strings.Replace(server.URL, "http://", "ws://", 1), NodeSecret: "node-secret",
		NodeType: "NODE_ROLE_EDGE", DiskSerial: "disk-123", Version: "1.0", Region: "Sydney",
		Capacity: 100, HeartbeatInterval: 20 * time.Millisecond, WebRTCBaseURL: "https://edge.example",
		PublishURL: "https://publish.example", ABRNegotiationAddress: "abr.example:443",
	}, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), pipelines...)
	}, testLogger{})
	defer client.Close()

	registration := decodeMessage(t, awaitMessage(t, messages))
	require.Equal(t, messageFields{
		msgType: "COMMAND_TYPE_REGISTER", nodeSecret: "node-secret", nodeType: "NODE_ROLE_EDGE",
		diskSerial: "disk-123", version: "1.0", region: "Sydney", webRTCBaseURL: "https://edge.example",
		publishURL: "https://publish.example", abrAddress: "abr.example:443",
	}, registration)

	mu.Lock()
	pipelines = []string{"app/c"}
	mu.Unlock()
	heartbeat := decodeMessage(t, awaitMessage(t, messages))
	require.Equal(t, "COMMAND_TYPE_HEARTBEAT", heartbeat.msgType)
	require.Equal(t, int32(100), heartbeat.capacity)
	require.Equal(t, []string{"app/c"}, heartbeat.pipelines)
	require.Empty(t, heartbeat.nodeSecret)
	require.Empty(t, heartbeat.nodeType)
	require.Empty(t, heartbeat.region)
}

func TestClientWaitsForAcceptedRegistration(t *testing.T) {
	heartbeat := make(chan struct{}, 1)
	server := newControlServer(t, func(conn *websocket.Conn) {
		_, _, err := conn.ReadMessage()
		if err != nil {
			return
		}
		time.Sleep(80 * time.Millisecond)
		_ = conn.WriteMessage(websocket.BinaryMessage, registerAck(true, ""))
		if _, _, err = conn.ReadMessage(); err == nil {
			heartbeat <- struct{}{}
		}
	})
	defer server.Close()
	client := newTestClient(server.URL)
	defer client.Close()

	select {
	case <-heartbeat:
		t.Fatal("heartbeat sent before registration was accepted")
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case <-heartbeat:
	case <-time.After(time.Second):
		t.Fatal("heartbeat not sent after registration was accepted")
	}
}

func TestClientReconnectsAfterRejectedRegistration(t *testing.T) {
	connections := make(chan struct{}, 2)
	server := newControlServer(t, func(conn *websocket.Conn) {
		connections <- struct{}{}
		_, _, err := conn.ReadMessage()
		if err == nil {
			_ = conn.WriteMessage(websocket.BinaryMessage, registerAck(false, "invalid secret"))
		}
	})
	defer server.Close()
	client := newTestClient(server.URL)
	defer client.Close()
	awaitConnection(t, connections)
	awaitConnection(t, connections)
}

func TestClientSendsNodeSecretAuthorization(t *testing.T) {
	header := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header <- r.Header.Get("Authorization")
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()
	client := newTestClient(server.URL)
	defer client.Close()
	require.Equal(t, "Bearer node-secret", <-header)
}

func TestDeriveFallbackURL(t *testing.T) {
	require.Equal(t, "http://127.0.0.1:18090/internal/mmx/v1", DeriveFallbackURL("ws://127.0.0.1:18090/ws/mmx"))
	require.Equal(t, "https://api.pp-cdn.org/internal/mmx/v1", DeriveFallbackURL("wss://api.pp-cdn.org/ws/mmx"))
	require.Empty(t, DeriveFallbackURL("://not-a-url"))
}

// TestClientRetriesWebSocketWhileFallbackHealthy is R1's acceptance test: a
// WebSocket dial that always fails, with an HTTP fallback that always
// succeeds, must still see repeated dial attempts. Before the fix, the
// fallback loop's only exit was a fallback failure, so a healthy fallback
// parked the client on it forever and connect() was never called again.
func TestClientRetriesWebSocketWhileFallbackHealthy(t *testing.T) {
	var mu sync.Mutex
	dialAttempts := 0
	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		dialAttempts++
		mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer wsServer.Close()

	fallback := newFallbackServer(t, nil)
	defer fallback.Close()

	client := New(context.Background(), Config{
		URL: strings.Replace(wsServer.URL, "http://", "ws://", 1), NodeSecret: "node-secret",
		NodeType: "NODE_ROLE_EDGE", Region: "Tokyo", Capacity: 10, HeartbeatInterval: 15 * time.Millisecond,
	}, func() []string { return nil }, testLogger{})
	client.SetHTTPFallback(fallback.URL, "node-secret", time.Second)
	defer client.Close()

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return dialAttempts >= 3
	}, 2*time.Second, 10*time.Millisecond,
		"WebSocket dial should be retried periodically while HTTP fallback stays healthy (R1)")
}

// TestClientReturnsToWebSocketAfterFallback is R1's second acceptance test:
// the WebSocket fails, the client rides out a couple of HTTP-fallback
// stretches, then the WebSocket starts accepting again - the client must
// notice and register over it, and the HTTP fallback heartbeats along the
// way must stay evenly spaced (no gap wide enough to trip ppcenter's
// heartbeatTimeout).
func TestClientReturnsToWebSocketAfterFallback(t *testing.T) {
	var attemptMu sync.Mutex
	dialAttempts := 0
	registered := make(chan struct{}, 1)
	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attemptMu.Lock()
		dialAttempts++
		attempt := dialAttempts
		attemptMu.Unlock()

		if attempt <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.BinaryMessage, registerAck(true, ""))
		select {
		case registered <- struct{}{}:
		default:
		}
		_, _, _ = conn.ReadMessage()
	}))
	defer wsServer.Close()

	var timesMu sync.Mutex
	var fallbackHeartbeats []time.Time
	fallback := newFallbackServer(t, func(path string) {
		if path == "/heartbeat" {
			timesMu.Lock()
			fallbackHeartbeats = append(fallbackHeartbeats, time.Now())
			timesMu.Unlock()
		}
	})
	defer fallback.Close()

	heartbeatInterval := 15 * time.Millisecond
	client := New(context.Background(), Config{
		URL: strings.Replace(wsServer.URL, "http://", "ws://", 1), NodeSecret: "node-secret",
		NodeType: "NODE_ROLE_EDGE", Region: "Tokyo", Capacity: 10, HeartbeatInterval: heartbeatInterval,
	}, func() []string { return nil }, testLogger{})
	client.SetHTTPFallback(fallback.URL, "node-secret", time.Second)
	defer client.Close()

	select {
	case <-registered:
	case <-time.After(3 * time.Second):
		t.Fatal("client never reconnected over WebSocket after riding out HTTP fallback")
	}

	timesMu.Lock()
	times := append([]time.Time(nil), fallbackHeartbeats...)
	timesMu.Unlock()
	require.NotEmpty(t, times, "fallback heartbeat should have fired before WebSocket recovered")
	for i := 1; i < len(times); i++ {
		gap := times[i].Sub(times[i-1])
		require.Lessf(t, gap, 4*heartbeatInterval,
			"fallback heartbeat gap %v at index %d is too wide - heartbeat must not be interrupted while WebSocket is retried", gap, i)
	}
}

// TestClientResetsBackoffAfterStableSession is R3's acceptance test: two
// quick WebSocket failures grow the reconnect backoff, then a session that
// stays registered long enough to count as stable (stableSessionCycles *
// HeartbeatInterval) must reset it back to its 1s floor rather than leaving
// it parked at the elevated value.
func TestClientResetsBackoffAfterStableSession(t *testing.T) {
	heartbeatInterval := 20 * time.Millisecond
	var mu sync.Mutex
	var dialTimes []time.Time
	attempt := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempt++
		n := attempt
		dialTimes = append(dialTimes, time.Now())
		mu.Unlock()

		if n <= 2 || n >= 4 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.BinaryMessage, registerAck(true, ""))
		time.Sleep(stableSessionCycles*heartbeatInterval + 20*time.Millisecond)
	}))
	defer server.Close()

	client := New(context.Background(), Config{
		URL: strings.Replace(server.URL, "http://", "ws://", 1), NodeSecret: "node-secret",
		NodeType: "NODE_ROLE_EDGE", Region: "Tokyo", Capacity: 10, HeartbeatInterval: heartbeatInterval,
	}, func() []string { return nil }, testLogger{})
	defer client.Close()

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return attempt >= 4
	}, 15*time.Second, 20*time.Millisecond, "expected at least 4 WebSocket dial attempts")

	mu.Lock()
	times := append([]time.Time(nil), dialTimes...)
	mu.Unlock()
	require.Len(t, times, 4)

	gapAfterStable := times[3].Sub(times[2])
	require.Lessf(t, gapAfterStable, 2500*time.Millisecond,
		"backoff should reset to ~1s after a stable session instead of staying at the ~4s it would otherwise have grown to (gap was %v)", gapAfterStable)
}

func newControlServer(t *testing.T, handle func(*websocket.Conn)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		handle(conn)
	}))
}

// newFallbackServer serves the two HTTP-fallback endpoints HTTPFallbackClient
// calls, always succeeding. onRequest, if non-nil, is invoked with the
// request path before the response is written.
func newFallbackServer(t *testing.T, onRequest func(path string)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if onRequest != nil {
			onRequest(r.URL.Path)
		}
		switch r.URL.Path {
		case "/heartbeat":
			w.WriteHeader(http.StatusOK)
		case "/commands/pending":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"commands":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func newTestClient(serverURL string) *Client {
	return New(context.Background(), Config{
		URL: strings.Replace(serverURL, "http://", "ws://", 1), NodeSecret: "node-secret",
		NodeType: "NODE_ROLE_ORIGIN", Region: "Tokyo", Capacity: 10, HeartbeatInterval: 20 * time.Millisecond,
	}, func() []string { return nil }, testLogger{})
}

func registerAck(accepted bool, reason string) []byte {
	out := appendString(nil, 1, "COMMAND_TYPE_REGISTER_ACK")
	out = protowire.AppendTag(out, 2, protowire.VarintType)
	if accepted {
		out = protowire.AppendVarint(out, 1)
	} else {
		out = protowire.AppendVarint(out, 0)
	}
	return appendString(out, 3, reason)
}

func awaitMessage(t *testing.T, messages <-chan []byte) []byte {
	t.Helper()
	select {
	case message := <-messages:
		return message
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for message")
		return nil
	}
}

func awaitConnection(t *testing.T, connections <-chan struct{}) {
	t.Helper()
	select {
	case <-connections:
	case <-time.After(4 * time.Second):
		t.Fatal("timed out waiting for connection")
	}
}

type messageFields struct {
	msgType, nodeSecret, nodeType, diskSerial, version, region string
	webRTCBaseURL, publishURL, abrAddress                      string
	capacity                                                   int32
	pipelines                                                  []string
}

func decodeMessage(t *testing.T, payload []byte) messageFields {
	t.Helper()
	var fields messageFields
	for len(payload) > 0 {
		number, wireType, n := protowire.ConsumeTag(payload)
		require.Greater(t, n, 0)
		payload = payload[n:]
		if wireType == protowire.VarintType {
			value, n := protowire.ConsumeVarint(payload)
			require.Greater(t, n, 0)
			payload = payload[n:]
			if number == 2 {
				fields.capacity = int32(value)
			}
			continue
		}
		value, n := protowire.ConsumeString(payload)
		require.Greater(t, n, 0)
		payload = payload[n:]
		if fields.msgType == "COMMAND_TYPE_HEARTBEAT" && number == 3 {
			fields.pipelines = append(fields.pipelines, value)
			continue
		}
		switch number {
		case 1:
			fields.msgType = value
		case 2:
			fields.nodeSecret = value
		case 3:
			fields.nodeType = value
		case 4:
			fields.diskSerial = value
		case 5:
			fields.version = value
		case 6:
			fields.region = value
		case 7:
			fields.webRTCBaseURL = value
		case 8:
			fields.publishURL = value
		case 9:
			fields.abrAddress = value
		}
	}
	return fields
}
