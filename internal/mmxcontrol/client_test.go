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

func TestClientSendsRegistrationAndHeartbeat(t *testing.T) {
	messages := make(chan []byte, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()
		for {
			messageType, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			require.Equal(t, websocket.BinaryMessage, messageType)
			messages <- payload
		}
	}))
	defer server.Close()

	var mu sync.Mutex
	pipelines := []string{"app/b", "app/a", "app/a"}
	client := New(context.Background(), Config{
		URL:  strings.Replace(server.URL, "http://", "ws://", 1),
		Role: "NODE_ROLE_EDGE", NodeID: 7, Version: "1.0", Region: "Sydney",
		Capacity: 100, HeartbeatInterval: 20 * time.Millisecond,
		WebRTCBaseURL: "https://edge.example",
	}, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), pipelines...)
	}, testLogger{})
	defer client.Close()

	fields := decodeIndication(t, awaitMessage(t, messages))
	require.Equal(t, "COMMAND_TYPE_INDICATION", fields.msgType)
	require.Equal(t, "NODE_ROLE_EDGE", fields.role)
	require.Equal(t, int32(7), fields.nodeID)
	require.Equal(t, []string{"app/a", "app/b"}, fields.pipelines)
	require.Equal(t, "https://edge.example", fields.webRTCBaseURL)

	mu.Lock()
	pipelines = []string{"app/c"}
	mu.Unlock()
	require.Equal(t, []string{"app/c"}, decodeIndication(t, awaitMessage(t, messages)).pipelines)
}

func TestClientSendsAuthorizationHeader(t *testing.T) {
	headers := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Get("Authorization")
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()

	client := New(context.Background(), Config{
		URL:       strings.Replace(server.URL, "http://", "ws://", 1),
		AuthToken: "s3cr3t-node-token",
		Role:      "NODE_ROLE_ORIGIN", NodeID: 1, Region: "Tokyo",
		Capacity: 10, HeartbeatInterval: time.Second,
	}, func() []string { return nil }, testLogger{})
	defer client.Close()

	select {
	case got := <-headers:
		require.Equal(t, "Bearer s3cr3t-node-token", got)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for dial")
	}
}

func TestClientReconnects(t *testing.T) {
	connections := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		require.NoError(t, err)
		connections <- struct{}{}
		_, _, _ = conn.ReadMessage()
		conn.Close()
	}))
	defer server.Close()

	client := New(context.Background(), Config{
		URL:  strings.Replace(server.URL, "http://", "ws://", 1),
		Role: "NODE_ROLE_ORIGIN", NodeID: 1, Region: "Tokyo",
		Capacity: 10, HeartbeatInterval: time.Second,
	}, func() []string { return nil }, testLogger{})
	defer client.Close()

	awaitConnection(t, connections)
	awaitConnection(t, connections)
}

func awaitMessage(t *testing.T, messages <-chan []byte) []byte {
	t.Helper()
	select {
	case message := <-messages:
		return message
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for indication")
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

type indicationFields struct {
	msgType       string
	role          string
	nodeID        int32
	pipelines     []string
	webRTCBaseURL string
}

func decodeIndication(t *testing.T, payload []byte) indicationFields {
	t.Helper()
	var fields indicationFields
	for len(payload) > 0 {
		number, wireType, consumed := protowire.ConsumeTag(payload)
		require.Greater(t, consumed, 0)
		payload = payload[consumed:]
		switch wireType {
		case protowire.BytesType:
			value, n := protowire.ConsumeString(payload)
			require.Greater(t, n, 0)
			payload = payload[n:]
			switch number {
			case 1:
				fields.msgType = value
			case 2:
				fields.role = value
			case 7:
				fields.pipelines = append(fields.pipelines, value)
			case 8:
				fields.webRTCBaseURL = value
			}
		case protowire.VarintType:
			value, n := protowire.ConsumeVarint(payload)
			require.Greater(t, n, 0)
			payload = payload[n:]
			if number == 3 {
				fields.nodeID = int32(value >> 1)
			}
		default:
			t.Fatalf("unsupported wire type %v", wireType)
		}
	}
	return fields
}
