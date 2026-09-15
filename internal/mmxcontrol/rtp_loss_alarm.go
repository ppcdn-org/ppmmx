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

// RTPLossAlarmReport is one WHIP publish session's most recent
// recvstats.Interval RTP loss-rate sample, matching ppcenter's
// POST /internal/mmx/v1/alarms/rtp-loss. Node identity is deliberately NOT
// included here - the nodeSecret bearer token this request already
// authenticates with resolves to the reporting node's identity server-side
// (nodeSecretAuthMiddleware), the same way SRTLossAlarmReport's is.
type RTPLossAlarmReport struct {
	PathName     string  `json:"pathName"`
	LossPct      float64 `json:"lossPct"`
	BitrateBps   float64 `json:"bitrateBps"`
	SustainedSec int     `json:"sustainedSec"` // continuous seconds over threshold; 0 on the one resolve report
}

// RTPLossAlarmClient reports WHIP ingest RTP loss-rate samples to ppcenter.
// Reuses mmxControl's own endpoint/credential (MMXControlURL +
// MMXNodeSecret), same as SRTLossAlarmClient.
type RTPLossAlarmClient struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewRTPLossAlarmClient(baseURL, token string, timeout time.Duration) *RTPLossAlarmClient {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &RTPLossAlarmClient{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:   token,
		client:  &http.Client{Timeout: timeout},
	}
}

// Report sends one loss-rate sample. Unlike TrafficUsageClient.Report, a
// zero LossPct is not skipped - it is the legitimate "loss dropped back
// under threshold" resolve report, not a no-op delta.
func (c *RTPLossAlarmClient) Report(ctx context.Context, report RTPLossAlarmReport) error {
	if c.baseURL == "" || c.token == "" {
		return fmt.Errorf("rtp loss alarm endpoint and bearer token are required")
	}
	if strings.TrimSpace(report.PathName) == "" || report.LossPct < 0 {
		return fmt.Errorf("invalid rtp loss alarm report")
	}

	encoded, err := json.Marshal(report)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/alarms/rtp-loss", bytes.NewReader(encoded))
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
		return fmt.Errorf("rtp loss alarm report returned status %d", response.StatusCode)
	}
	return nil
}
