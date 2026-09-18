package mmxcontrol

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bluenviron/mediamtx/internal/healthlog"
)

// HealthLogClient reports batched structured health log events to ppcenter's
// POST /internal/mmx/v1/log-events. Reuses mmxControl's own endpoint and
// credential (MMXControlURL + MMXNodeSecret), same as every other
// node-to-ppcenter reporter - node identity is resolved server-side from the
// bearer, so the payload carries none.
//
// Bodies are gzip-compressed: a batch of events is considerably larger than
// the other single-record reports on this channel, and the contract
// advertises Content-Encoding: gzip.
type HealthLogClient struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewHealthLogClient(baseURL, token string, timeout time.Duration) *HealthLogClient {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &HealthLogClient{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:   token,
		client:  &http.Client{Timeout: timeout},
	}
}

type healthLogReportRequest struct {
	Events []healthlog.Event `json:"events"`
}

// ReportEvents implements healthlog.Reporter.
func (c *HealthLogClient) ReportEvents(ctx context.Context, events []healthlog.Event) error {
	if len(events) == 0 {
		return nil
	}
	if c.baseURL == "" || c.token == "" {
		return fmt.Errorf("health log endpoint and bearer token are required")
	}

	encoded, err := json.Marshal(healthLogReportRequest{Events: events})
	if err != nil {
		return err
	}

	var body bytes.Buffer
	gz := gzip.NewWriter(&body)
	if _, err := gz.Write(encoded); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/log-events", &body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Content-Encoding", "gzip")

	response, err := c.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("health log report returned status %d", response.StatusCode)
	}
	return nil
}
