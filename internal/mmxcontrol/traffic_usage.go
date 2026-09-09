package mmxcontrol

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// TrafficUsageReport is one incremental downstream-byte sample for a single
// appId/day, matching ppcenter's POST /internal/mmx/v1/traffic/usage (see
// docs/design/ppcdn-billing-and-traffic-monitoring.zh-CN.md §2.6 item 1).
// DownstreamDeltaBytes is a delta since the last report for this
// (appId, usageDate) pair, not a running total - the sampler that produces
// these (see internal/core/traffic_usage_sampler.go) is responsible for
// tracking each session's last-seen cumulative byte count itself.
type TrafficUsageReport struct {
	AppID                string `json:"appId"`
	UsageDate            string `json:"usageDate"` // YYYY-MM-DD, UTC
	DownstreamDeltaBytes int64  `json:"downstreamDeltaBytes"`
}

// TrafficUsageClient reports downstream byte usage to ppcenter. Reuses
// mmxControl's own endpoint/credential (MMXControlURL + MMXNodeSecret),
// same as the split-rec file reporter in core.go - every deployment with
// mmxControl on gets usage reporting for free, no separate opt-in flag.
type TrafficUsageClient struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewTrafficUsageClient(baseURL, token string, timeout time.Duration) *TrafficUsageClient {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &TrafficUsageClient{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:   token,
		client:  &http.Client{Timeout: timeout},
	}
}

// Report sends one incremental sample. Under-reporting on failure (rather
// than retrying, which risks double-counting a delta that already landed)
// is the deliberate choice here - see the handler-side comment on
// ppcenter's TrafficUsageHandlerV1.Report.
func (c *TrafficUsageClient) Report(ctx context.Context, report TrafficUsageReport) error {
	if c.baseURL == "" || c.token == "" {
		return fmt.Errorf("traffic usage endpoint and bearer token are required")
	}
	if report.AppID == "" || report.UsageDate == "" || report.DownstreamDeltaBytes < 0 {
		return fmt.Errorf("invalid traffic usage report")
	}
	if report.DownstreamDeltaBytes == 0 {
		return nil
	}

	encoded, err := json.Marshal(report)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/traffic/usage", bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")

	response, err := c.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("traffic usage report returned status %d", response.StatusCode)
	}
	return nil
}
