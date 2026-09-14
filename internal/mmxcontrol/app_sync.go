package mmxcontrol

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// AppCredential is one app ppcenter currently reports as eligible to
// publish, as returned by AppSyncClient.Fetch.
type AppCredential struct {
	AppID     string
	AppSecret string
}

// AppSyncClient pulls the current set of apps allowed to publish from
// ppcenter's GET /internal/mmx/v1/app-credentials/sync (see
// docs/design/ppcdn-mmx-publish-whitelist.zh-CN.md). Reuses mmxControl's
// own endpoint/credential (MMXControlURL + MMXNodeSecret), same as
// TrafficUsageClient and the split-rec file reporter in core.go.
type AppSyncClient struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewAppSyncClient(baseURL, token string, timeout time.Duration) *AppSyncClient {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &AppSyncClient{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:   token,
		client:  &http.Client{Timeout: timeout},
	}
}

// Fetch returns every app ppcenter currently reports as eligible to
// publish (active, non-arrears - see AppSyncHandlerV1.Sync), including its
// appSecret so split-rec (see recording.md §2.3) can verify a request's
// signature against the caller's own app credential instead of a single
// deployment-wide shared secret. The caller is expected to poll this
// periodically and replace its own cached set wholesale with the result:
// an app that stops appearing (deleted, arrears) is meant to lose publish
// access within one poll interval without ppcenter needing to push an
// explicit revocation.
func (c *AppSyncClient) Fetch(ctx context.Context) ([]AppCredential, error) {
	if c.baseURL == "" || c.token == "" {
		return nil, fmt.Errorf("app sync endpoint and bearer token are required")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/app-credentials/sync", nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)

	response, err := c.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("app sync request returned status %d", response.StatusCode)
	}

	var body struct {
		Apps []struct {
			AppID     string `json:"appId"`
			AppSecret string `json:"appSecret"`
		} `json:"apps"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("invalid JSON from app sync endpoint: %w", err)
	}
	creds := make([]AppCredential, 0, len(body.Apps))
	for _, app := range body.Apps {
		if app.AppID != "" {
			creds = append(creds, AppCredential{AppID: app.AppID, AppSecret: app.AppSecret})
		}
	}
	return creds, nil
}
