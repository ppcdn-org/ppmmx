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

// RTPLossSampleReport is one WHIP publish session's per-recvstats.Interval
// RTP loss-rate sample, matching ppcenter's
// POST /internal/mmx/v1/rtp-loss-samples. Node identity is deliberately NOT
// included - the nodeSecret bearer token this request authenticates with
// resolves to the reporting node server-side, same as RTPLossAlarmReport.
type RTPLossSampleReport struct {
	PathName   string  `json:"pathName"`
	LossPct    float64 `json:"lossPct"`
	BitrateBps float64 `json:"bitrateBps"`
	// MinuteUTC is "YYYY-MM-DD HH:mm" UTC; ppcenter falls back to its own
	// current minute if empty/unparseable.
	MinuteUTC string `json:"minuteUtc"`
}

// RTPLossSampleClient reports WHIP ingest RTP loss-rate samples to ppcenter
// every recvstats.Interval, independent of the threshold-gated RTP loss
// ALARM (RTPLossAlarmClient) - this is the full per-minute series behind the
// unified loss_samples table. Reuses mmxControl's endpoint/credential
// (MMXControlURL + MMXNodeSecret), same as the alarm client.
type RTPLossSampleClient struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewRTPLossSampleClient(baseURL, token string, timeout time.Duration) *RTPLossSampleClient {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &RTPLossSampleClient{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:   token,
		client:  &http.Client{Timeout: timeout},
	}
}

// Report sends one per-minute loss sample. ppcenter records only samples
// above its persistence threshold and treats the rest as a successful no-op,
// so callers should send every interval's sample, not just the bad ones.
func (c *RTPLossSampleClient) Report(ctx context.Context, report RTPLossSampleReport) error {
	if c.baseURL == "" || c.token == "" {
		return fmt.Errorf("rtp loss sample endpoint and bearer token are required")
	}
	if strings.TrimSpace(report.PathName) == "" || report.LossPct < 0 {
		return fmt.Errorf("invalid rtp loss sample report")
	}

	encoded, err := json.Marshal(report)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/rtp-loss-samples", bytes.NewReader(encoded))
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
		return fmt.Errorf("rtp loss sample report returned status %d", response.StatusCode)
	}
	return nil
}
