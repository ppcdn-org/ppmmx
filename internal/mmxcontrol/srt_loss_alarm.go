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

// SRTLossAlarmReport is one SRT publish connection's most recent
// recvstats.Interval loss-rate sample, matching ppcenter's
// POST /internal/mmx/v1/alarms/srt-loss. Node identity is deliberately NOT
// included here - the nodeSecret bearer token this request already
// authenticates with resolves to the reporting node's identity server-side
// (nodeSecretAuthMiddleware), the same way TrafficUsageReport's per-node
// byte rollup is attributed without the node claiming its own identity in
// the body.
type SRTLossAlarmReport struct {
	PathName     string  `json:"pathName"`
	LossPct      float64 `json:"lossPct"`
	BitrateBps   float64 `json:"bitrateBps"`
	SustainedSec int     `json:"sustainedSec"` // continuous seconds over threshold; 0 on the one resolve report
}

// SRTLossAlarmClient reports SRT ingest loss-rate samples to ppcenter.
// Reuses mmxControl's own endpoint/credential (MMXControlURL +
// MMXNodeSecret), same as TrafficUsageClient.
type SRTLossAlarmClient struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewSRTLossAlarmClient(baseURL, token string, timeout time.Duration) *SRTLossAlarmClient {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &SRTLossAlarmClient{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:   token,
		client:  &http.Client{Timeout: timeout},
	}
}

// Report sends one loss-rate sample. Unlike TrafficUsageClient.Report, a
// zero LossPct is not skipped - it is the legitimate "loss dropped back
// under threshold" resolve report, not a no-op delta.
func (c *SRTLossAlarmClient) Report(ctx context.Context, report SRTLossAlarmReport) error {
	if c.baseURL == "" || c.token == "" {
		return fmt.Errorf("srt loss alarm endpoint and bearer token are required")
	}
	if strings.TrimSpace(report.PathName) == "" || report.LossPct < 0 {
		return fmt.Errorf("invalid srt loss alarm report")
	}

	encoded, err := json.Marshal(report)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/alarms/srt-loss", bytes.NewReader(encoded))
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
		return fmt.Errorf("srt loss alarm report returned status %d", response.StatusCode)
	}
	return nil
}
