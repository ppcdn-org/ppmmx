package mmxcontrol

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bluenviron/mediamtx/internal/logger"
)

type HTTPFallbackClient struct {
	baseURL  string
	token    string
	client   *http.Client
	indicate func() []byte
	logParent logger.Writer
}

func NewHTTPFallbackClient(baseURL, token string, timeout time.Duration, indicate func() []byte, logParent logger.Writer) *HTTPFallbackClient {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &HTTPFallbackClient{
		baseURL:  strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:    token,
		client:   &http.Client{Timeout: timeout},
		indicate: indicate,
		logParent: logParent,
	}
}

func (h *HTTPFallbackClient) Heartbeat(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.baseURL+"/heartbeat", bytes.NewReader(h.indicate()))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fallback heartbeat returned status %d", resp.StatusCode)
	}
	return nil
}

func (h *HTTPFallbackClient) PollCommands(ctx context.Context) ([]struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Payload string `json:"payload,omitempty"`
}, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.baseURL+"/commands/pending", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fallback commands returned status %d", resp.StatusCode)
	}
	var body struct {
		Commands []struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Payload string `json:"payload,omitempty"`
		} `json:"commands"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Commands, nil
}
