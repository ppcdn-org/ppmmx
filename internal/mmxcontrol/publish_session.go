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

// PublishSessionStartReport announces that a WHIP publish session went
// live, matching ppcenter's POST /internal/mmx/v1/publish-sessions/start.
//
// SessionID is the WHIP session's UUID and is the correlation key between
// this and the matching end report - ppcenter keys its history row on it,
// so the two calls must carry the same value.
//
// PathName is "{appId}/{streamName}[/{codec}]" (see the HEVC/H264
// multitrack design doc); ppcenter parses the appId out of it to attribute
// the session to an account, the same convention traffic usage already
// relies on.
type PublishSessionStartReport struct {
	SessionID  string    `json:"sessionId"`
	PathName   string    `json:"pathName"`
	RemoteAddr string    `json:"remoteAddr,omitempty"`
	UserAgent  string    `json:"userAgent,omitempty"`
	StartedAt  time.Time `json:"startedAt"`
}

// PublishSessionEndReport closes out a session started by a
// PublishSessionStartReport with the same SessionID, matching ppcenter's
// POST /internal/mmx/v1/publish-sessions/end.
//
// InboundBytes is the session's cumulative received byte count, not a
// delta - unlike traffic usage this is reported exactly once per session,
// so there is nothing to accumulate against.
type PublishSessionEndReport struct {
	SessionID    string    `json:"sessionId"`
	EndedAt      time.Time `json:"endedAt"`
	EndReason    string    `json:"endReason,omitempty"`
	InboundBytes uint64    `json:"inboundBytes"`
}

// PublishSessionClient reports publish session lifecycle events to
// ppcenter. Reuses mmxControl's own endpoint/credential (MMXControlURL +
// MMXNodeSecret), same as the traffic usage and split-rec reporters - every
// deployment with mmxControl on gets publish history for free, with no
// separate opt-in flag.
type PublishSessionClient struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewPublishSessionClient(baseURL, token string, timeout time.Duration) *PublishSessionClient {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &PublishSessionClient{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:   token,
		client:  &http.Client{Timeout: timeout},
	}
}

// ReportPublishStart sends the start event.
func (c *PublishSessionClient) ReportPublishStart(
	ctx context.Context,
	sessionID, pathName, remoteAddr, userAgent string,
	startedAt time.Time,
) error {
	if sessionID == "" || pathName == "" {
		return fmt.Errorf("sessionId and pathName are required")
	}
	return c.post(ctx, "/publish-sessions/start", PublishSessionStartReport{
		SessionID:  sessionID,
		PathName:   pathName,
		RemoteAddr: remoteAddr,
		UserAgent:  userAgent,
		StartedAt:  startedAt.UTC(),
	})
}

// ReportPublishEnd sends the end event for a previously started session.
//
// A 404 is deliberately not treated as an error worth retrying: it means
// ppcenter has no record of the start (e.g. it was unreachable at the time,
// or was restarted mid-session), and re-sending the end would not create
// one. The caller logs and moves on.
func (c *PublishSessionClient) ReportPublishEnd(
	ctx context.Context,
	sessionID string,
	endedAt time.Time,
	endReason string,
	inboundBytes uint64,
) error {
	if sessionID == "" {
		return fmt.Errorf("sessionId is required")
	}
	return c.post(ctx, "/publish-sessions/end", PublishSessionEndReport{
		SessionID:    sessionID,
		EndedAt:      endedAt.UTC(),
		EndReason:    endReason,
		InboundBytes: inboundBytes,
	})
}

func (c *PublishSessionClient) post(ctx context.Context, path string, payload any) error {
	if c.baseURL == "" || c.token == "" {
		return fmt.Errorf("publish session endpoint and bearer token are required")
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(encoded))
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
		return fmt.Errorf("publish session report returned status %d", response.StatusCode)
	}
	return nil
}
