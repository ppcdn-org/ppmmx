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

// RecordDeletionClaim is one object pending removal from storage, returned
// by the claim endpoint.
type RecordDeletionClaim struct {
	ID        uint64 `json:"id"`
	AppID     string `json:"appId"`
	ObjectKey string `json:"objectKey"`
	AppEnv    string `json:"appEnv"`
	SizeBytes int64  `json:"sizeBytes"`
	Status    string `json:"status"`
	Attempts  int    `json:"attempts"`
}

// RecordDeletionResult is the outcome the node reports back.
type RecordDeletionResult struct {
	ID      uint64 `json:"id"`
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// RecordDeletionClient claims pending object deletions from ppcenter and
// reports their outcomes. Reuses mmxControl's own endpoint/credential
// (MMXControlURL + MMXNodeSecret), same as every other node-to-ppcenter
// reporter.
type RecordDeletionClient struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewRecordDeletionClient(baseURL, token string, timeout time.Duration) *RecordDeletionClient {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &RecordDeletionClient{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:   token,
		client:  &http.Client{Timeout: timeout},
	}
}

// Claim retrieves up to limit pending object deletions for this node to
// execute. Returns an empty slice when there is nothing to do.
func (c *RecordDeletionClient) Claim(ctx context.Context, limit int) ([]RecordDeletionClaim, error) {
	if c.baseURL == "" || c.token == "" {
		return nil, fmt.Errorf("deletion endpoint and bearer token are required")
	}
	u := c.baseURL + "/records/deletions/claim"
	if limit > 0 {
		u += fmt.Sprintf("?limit=%d", limit)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
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
		return nil, fmt.Errorf("claim returned status %d", response.StatusCode)
	}

	var claims []RecordDeletionClaim
	if err := json.NewDecoder(response.Body).Decode(&claims); err != nil {
		return nil, err
	}
	return claims, nil
}

// Complete reports whether one claimed deletion succeeded or failed.
func (c *RecordDeletionClient) Complete(ctx context.Context, id uint64, success bool, errMsg string) error {
	if c.baseURL == "" || c.token == "" {
		return fmt.Errorf("deletion endpoint and bearer token are required")
	}
	body := RecordDeletionResult{ID: id, Success: success, Error: strings.TrimSpace(errMsg)}
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/records/deletions/complete", bytes.NewReader(encoded))
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
		return fmt.Errorf("complete returned status %d", response.StatusCode)
	}
	return nil
}
