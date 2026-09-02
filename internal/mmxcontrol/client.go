// Package mmxcontrol connects an MMX node to the PPCDN control plane.
package mmxcontrol

import (
	"bytes"
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/bluenviron/mediamtx/internal/logger"
)

var errReconnectHint = errors.New("RECONNECT_HINT received")

// Config contains the legacy ppcenter WorkerIndication identity.
type Config struct {
	URL                   string
	AuthToken             string // sent as "Authorization: Bearer <token>" on both the WS dial and the HTTP fallback; must match ppcenter's nodeAuth.bearerToken (or a nodeAuth.nodeTokens entry)
	Role                  string
	NodeID                int32
	Version               string
	Region                string
	Capacity              int32
	HeartbeatInterval     time.Duration
	WebRTCBaseURL         string
	PublishURL            string
	ABRNegotiationAddress string
	PoolID                string
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
		c.fallback = NewHTTPFallbackClient(baseURL, token, timeout, c.indication, c.parent)
	}
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
		c.parent.Log(logger.Warn, "MMX control connection lost: %v", err)

		if c.fallback != nil {
			c.parent.Log(logger.Info, "MMX control falling back to HTTP")
			for {
				if ctx.Err() != nil {
					return
				}
				if hbErr := c.fallback.Heartbeat(ctx); hbErr != nil {
					c.parent.Log(logger.Warn, "MMX HTTP fallback heartbeat failed: %v", hbErr)
					break
				}
				if _, pollErr := c.fallback.PollCommands(ctx); pollErr != nil {
					c.parent.Log(logger.Warn, "MMX HTTP fallback poll failed: %v", pollErr)
					break
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(c.config.HeartbeatInterval):
				}
			}
		}

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
	if c.config.AuthToken != "" {
		header = http.Header{"Authorization": []string{"Bearer " + c.config.AuthToken}}
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, c.config.URL, header)
	if err != nil {
		return err
	}
	defer conn.Close()
	c.parent.Log(logger.Info, "MMX control connected to %s", c.config.URL)

	readErr := make(chan error, 1)
	go func() {
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
		}
	}()

	send := func() error {
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return conn.WriteMessage(websocket.BinaryMessage, c.indication())
	}
	if err := send(); err != nil {
		return err
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
			if err := send(); err != nil {
				return err
			}
		}
	}
}

func (c *Client) indication() []byte {
	pipelines := append([]string(nil), c.snapshot()...)
	sort.Strings(pipelines)
	pipelines = deduplicate(pipelines)

	var out []byte
	out = appendString(out, 1, "COMMAND_TYPE_INDICATION")
	out = appendString(out, 2, c.config.Role)
	out = protowire.AppendTag(out, 3, protowire.VarintType)
	out = protowire.AppendVarint(out, uint64(uint32(c.config.NodeID)<<1))
	out = appendString(out, 4, c.config.Version)
	out = appendString(out, 5, c.config.Region)
	out = protowire.AppendTag(out, 6, protowire.VarintType)
	out = protowire.AppendVarint(out, uint64(c.config.Capacity))
	for _, pipeline := range pipelines {
		out = appendString(out, 7, pipeline)
	}
	if value := strings.TrimSpace(c.config.WebRTCBaseURL); value != "" {
		out = appendString(out, 8, value)
	}
	if value := strings.TrimSpace(c.config.PublishURL); value != "" {
		out = appendString(out, 9, value)
	}
	if value := strings.TrimSpace(c.config.ABRNegotiationAddress); value != "" {
		out = appendString(out, 10, value)
	}
	if value := strings.TrimSpace(c.config.PoolID); value != "" {
		out = appendString(out, 11, value)
	}
	return out
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
