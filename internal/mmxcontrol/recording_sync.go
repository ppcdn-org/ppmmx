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

type RecordingTask struct {
	ID                 uint64 `json:"id"`
	AppID              string `json:"appId"`
	StreamPath         string `json:"streamPath"`
	RecordingSessionID string `json:"recordingSessionId"`
	Status             string `json:"status"`
	OutputDir          string `json:"outputDir"`
	SegmentDurationSec int    `json:"segmentDurationSeconds"`
}

type SegmentMetadata struct {
	AppID      string `json:"appId"`
	StreamPath string `json:"streamPath"`
	RoundID    string `json:"roundId"`
	Sequence   int64  `json:"sequence"`
	FileName   string `json:"fileName"`
	LocalPath  string `json:"localPath,omitempty"`
	ObjectKey  string `json:"objectKey,omitempty"`
	UploadURL  string `json:"uploadUrl,omitempty"`
	DurationMS int64  `json:"durationMs"`
	SizeBytes  int64  `json:"sizeBytes"`
	Checksum   string `json:"checksum,omitempty"`
}

type RecordingSyncClient struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewRecordingSyncClient(baseURL, token string, timeout time.Duration) *RecordingSyncClient {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &RecordingSyncClient{baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"), token: token, client: &http.Client{Timeout: timeout}}
}

func (c *RecordingSyncClient) ActiveTasks(ctx context.Context) ([]RecordingTask, error) {
	request, err := c.request(ctx, http.MethodGet, "/records/tasks/active", nil)
	if err != nil {
		return nil, err
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("recording task poll returned status %d", response.StatusCode)
	}
	var tasks []RecordingTask
	if err := json.NewDecoder(response.Body).Decode(&tasks); err != nil {
		return nil, err
	}
	for _, task := range tasks {
		if task.AppID == "" || task.StreamPath == "" || task.RecordingSessionID == "" || task.Status != "active" {
			return nil, fmt.Errorf("recording task poll returned an invalid task")
		}
	}
	return tasks, nil
}

func (c *RecordingSyncClient) ReportSegment(ctx context.Context, segment SegmentMetadata) error {
	if segment.AppID == "" || segment.StreamPath == "" || segment.RoundID == "" || segment.FileName == "" || segment.Sequence < 0 || segment.DurationMS <= 0 || segment.SizeBytes < 0 {
		return fmt.Errorf("invalid recording segment metadata")
	}
	request, err := c.request(ctx, http.MethodPost, "/records/segments", segment)
	if err != nil {
		return err
	}
	response, err := c.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("recording segment report returned status %d", response.StatusCode)
	}
	return nil
}

func (c *RecordingSyncClient) request(ctx context.Context, method, path string, body any) (*http.Request, error) {
	if c.baseURL == "" || c.token == "" {
		return nil, fmt.Errorf("recording sync endpoint and bearer token are required")
	}
	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return request, nil
}
