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

// SRTLossSampleReport is one SRT publish connection's per-minute loss
// breakdown, matching ppcenter's POST /internal/mmx/v1/srt-loss-samples.
// Node identity is deliberately absent - the nodeSecret this request
// authenticates with resolves to the reporting node server-side, same as
// every other /internal/mmx/v1 report.
//
// Two loss figures, not one, because their ratio is the story:
// UnrecoverablePct is what a viewer experiences (ARQ gave up), and
// RecoverablePct is what the link lost but ARQ recovered in time. Their sum
// is the raw loss rate SRT reports.
type SRTLossSampleReport struct {
	PathName         string  `json:"pathName"`
	RecoverablePct   float64 `json:"recoverablePct"`
	UnrecoverablePct float64 `json:"unrecoverablePct"`
	BitrateBps       float64 `json:"bitrateBps"`
	// MinuteUTC is "YYYY-MM-DD HH:mm", the UTC minute this sample covers.
	MinuteUTC string `json:"minuteUtc"`
}

// SRTLossSampleClient reports per-minute SRT loss breakdowns to ppcenter.
// Reuses mmxControl's own endpoint/credential (MMXControlURL +
// MMXNodeSecret), same as TrafficUsageClient/SRTLossAlarmClient.
type SRTLossSampleClient struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewSRTLossSampleClient(baseURL, token string, timeout time.Duration) *SRTLossSampleClient {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &SRTLossSampleClient{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:   token,
		client:  &http.Client{Timeout: timeout},
	}
}

// Report sends one per-minute sample. Zero percentages are sent, not
// skipped: "no loss this minute" is a real and useful data point in a trend,
// unlike TrafficUsageClient's zero-delta no-op.
func (c *SRTLossSampleClient) Report(ctx context.Context, report SRTLossSampleReport) error {
	if c.baseURL == "" || c.token == "" {
		return fmt.Errorf("srt loss sample endpoint and bearer token are required")
	}
	if strings.Trim(strings.TrimSpace(report.PathName), "/") == "" {
		return fmt.Errorf("pathName is required")
	}
	if report.RecoverablePct < 0 || report.UnrecoverablePct < 0 || report.BitrateBps < 0 {
		return fmt.Errorf("invalid srt loss sample report")
	}

	encoded, err := json.Marshal(report)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/srt-loss-samples", bytes.NewReader(encoded))
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
		return fmt.Errorf("srt loss sample report returned status %d", response.StatusCode)
	}
	return nil
}
