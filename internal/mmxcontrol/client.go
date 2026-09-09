// Package mmxcontrol connects an MMX node to the PPCDN control plane.
package mmxcontrol

import (
	"bytes"
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/bluenviron/mediamtx/internal/logger"
)

var errReconnectHint = errors.New("RECONNECT_HINT received")

// Config contains the MMX node identity and static endpoint configuration.
type Config struct {
	URL                   string
	NodeSecret            string // sent as the Authorization bearer on MMX connections
	NodeType              string
	DiskSerial            string
	Version               string
	Region                string
	Capacity              int32
	HeartbeatInterval     time.Duration
	WebRTCBaseURL         string
	PublishURL            string
	ABRNegotiationAddress string
}

// Client sends registration and heartbeat indications to ppcenter.
type Client struct {
	config   Config
	snapshot func() []string
	parent   logger.Writer
	cancel   context.CancelFunc
	done     chan struct{}
	once     sync.Once
	fallback *HTTPFallbackClient

	// outageLogged suppresses the "connection lost" / "falling back to
	// HTTP" / "fallback heartbeat|poll failed" lines on retries after the
	// first one for an ongoing outage. Without this, a downed ppcenter
	// makes run() log the same triplet every backoff cycle (capped at
	// 30s) for as long as it stays down, saying nothing the first
	// occurrence didn't already say. Reset to false in connect() on a
	// successful dial, so the next outage logs again. Only ever touched
	// from the single goroutine run() spawns, so no lock needed.
	outageLogged bool
}

// New starts a control-plane client.
func New(parent context.Context, config Config, snapshot func() []string, logParent logger.Writer) *Client {
	ctx, cancel := context.WithCancel(parent)
	c := &Client{
		config: config, snapshot: snapshot, parent: logParent,
		cancel: cancel, done: make(chan struct{}),
	}
	go c.run(ctx)
	return c
}

// Close stops the client and waits for its goroutine.
func (c *Client) Close() {
	c.once.Do(c.cancel)
	<-c.done
}

func (c *Client) SetHTTPFallback(baseURL, token string, timeout time.Duration) {
	if baseURL != "" {
		c.fallback = NewHTTPFallbackClient(baseURL, token, timeout, c.heartbeat, c.parent)
	}
}

// DeriveFallbackURL builds the HTTP fallback base URL from the WS control
// URL's scheme and host. ppcenter serves the fallback trio at
// /internal/mmx/v1 (see registerNodeFallbackRoutes in ppcenter's route.go),
// not under the WS path, so the control URL's own path is discarded rather
// than reused. Returns "" if controlURL doesn't parse, which leaves the
// fallback client unset (SetHTTPFallback treats "" as "no fallback").
func DeriveFallbackURL(controlURL string) string {
	u, err := url.Parse(controlURL)
	if err != nil {
		return ""
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	}
	u.Path = "/internal/mmx/v1"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func (c *Client) run(ctx context.Context) {
	defer close(c.done)
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		err := c.connect(ctx)
		if ctx.Err() != nil {
			return
		}
		if !c.outageLogged {
			c.parent.Log(logger.Warn, "MMX control connection lost: %v", err)
		}

		if c.fallback != nil {
			if !c.outageLogged {
				c.parent.Log(logger.Info, "MMX control falling back to HTTP")
			}
			for {
				if ctx.Err() != nil {
					return
				}
				if hbErr := c.fallback.Heartbeat(ctx); hbErr != nil {
					if !c.outageLogged {
						c.parent.Log(logger.Warn, "MMX HTTP fallback heartbeat failed: %v", hbErr)
					}
					break
				}
				if _, pollErr := c.fallback.PollCommands(ctx); pollErr != nil {
					if !c.outageLogged {
						c.parent.Log(logger.Warn, "MMX HTTP fallback poll failed: %v", pollErr)
					}
					break
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(c.config.HeartbeatInterval):
				}
			}
		}
		c.outageLogged = true

		if !errors.Is(err, errReconnectHint) {
			delay := backoff + time.Duration(rand.Int64N(int64(backoff/2)+1))
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
	}
}

func (c *Client) connect(ctx context.Context) error {
	var header http.Header
	if c.config.NodeSecret != "" {
		header = http.Header{"Authorization": []string{"Bearer " + c.config.NodeSecret}}
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, c.config.URL, header)
	if err != nil {
		return err
	}
	defer conn.Close()
	c.parent.Log(logger.Info, "MMX control connected to %s", c.config.URL)
	c.outageLogged = false

	readErr := make(chan error, 1)
	ack := make(chan error, 1)
	go func() {
		registered := false
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				readErr <- err
				return
			}
			if bytes.Contains(data, []byte("RECONNECT_HINT")) {
				c.parent.Log(logger.Info, "MMX control received RECONNECT_HINT, reconnecting")
				readErr <- errReconnectHint
				return
			}
			if accepted, reason, ok := decodeNodeRegisterAck(data); ok && !registered {
				if accepted {
					registered = true
					ack <- nil
				} else {
					ack <- errors.New("MMX node registration rejected: " + reason)
					return
				}
			}
		}
	}()

	if err := func() error {
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return conn.WriteMessage(websocket.BinaryMessage, c.registration())
	}(); err != nil {
		return err
	}
	select {
	case err := <-ack:
		if err != nil {
			return err
		}
	case err := <-readErr:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}

	sendHeartbeat := func() error {
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return conn.WriteMessage(websocket.BinaryMessage, c.heartbeat())
	}

	ticker := time.NewTicker(c.config.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, "shutdown"), time.Now().Add(time.Second))
			return ctx.Err()
		case err := <-readErr:
			return err
		case <-ticker.C:
			if err := sendHeartbeat(); err != nil {
				return err
			}
		}
	}
}

func (c *Client) registration() []byte {
	var out []byte
	out = appendString(out, 1, "COMMAND_TYPE_REGISTER")
	out = appendString(out, 2, c.config.NodeSecret)
	out = appendString(out, 3, c.config.NodeType)
	out = appendString(out, 4, c.config.DiskSerial)
	out = appendString(out, 5, c.config.Version)
	out = appendString(out, 6, c.config.Region)
	if value := strings.TrimSpace(c.config.WebRTCBaseURL); value != "" {
		out = appendString(out, 7, value)
	}
	if value := strings.TrimSpace(c.config.PublishURL); value != "" {
		out = appendString(out, 8, value)
	}
	if value := strings.TrimSpace(c.config.ABRNegotiationAddress); value != "" {
		out = appendString(out, 9, value)
	}
	return out
}

func (c *Client) heartbeat() []byte {
	pipelines := append([]string(nil), c.snapshot()...)
	sort.Strings(pipelines)
	pipelines = deduplicate(pipelines)

	var out []byte
	out = appendString(out, 1, "COMMAND_TYPE_HEARTBEAT")
	out = protowire.AppendTag(out, 2, protowire.VarintType)
	out = protowire.AppendVarint(out, uint64(c.config.Capacity))
	for _, pipeline := range pipelines {
		out = appendString(out, 3, pipeline)
	}
	return out
}

func decodeNodeRegisterAck(data []byte) (accepted bool, reason string, ok bool) {
	var msgType string
	for len(data) > 0 {
		number, wireType, n := protowire.ConsumeTag(data)
		if n < 0 {
			return false, "", false
		}
		data = data[n:]
		switch number {
		case 1:
			value, n := protowire.ConsumeString(data)
			if n < 0 {
				return false, "", false
			}
			msgType = value
			data = data[n:]
		case 2:
			value, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return false, "", false
			}
			accepted = value != 0
			data = data[n:]
		case 3:
			value, n := protowire.ConsumeString(data)
			if n < 0 {
				return false, "", false
			}
			reason = value
			data = data[n:]
		default:
			n := protowire.ConsumeFieldValue(number, wireType, data)
			if n < 0 {
				return false, "", false
			}
			data = data[n:]
		}
	}
	return accepted, reason, msgType == "COMMAND_TYPE_REGISTER_ACK"
}

// DiskSerial returns a best-effort serial without invoking an external command.
func DiskSerial() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	entries, err := os.ReadDir("/sys/block")
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if value, err := os.ReadFile(filepath.Join("/sys/block", entry.Name(), "device", "serial")); err == nil && strings.TrimSpace(string(value)) != "" {
			return strings.TrimSpace(string(value))
		}
	}
	return ""
}

func appendString(out []byte, number protowire.Number, value string) []byte {
	out = protowire.AppendTag(out, number, protowire.BytesType)
	return protowire.AppendString(out, value)
}

func deduplicate(values []string) []string {
	result := values[:0]
	for _, value := range values {
		if value == "" || (len(result) > 0 && result[len(result)-1] == value) {
			continue
		}
		result = append(result, value)
	}
	return result
}
