package mmxcontrol

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type SourceResolution struct {
	Source          string
	WHEPBearerToken string
}

type cachedResolution struct {
	SourceResolution
	expiresAt time.Time
}

type OriginResolver struct {
	url, token, region string
	nodeID             int32
	client             *http.Client
	mu                 sync.Mutex
	cache              map[string]cachedResolution
}

func NewOriginResolver(endpoint, token, region string, nodeID int32, timeout time.Duration) *OriginResolver {
	return &OriginResolver{
		url: endpoint, token: token, region: region, nodeID: nodeID,
		client: &http.Client{Timeout: timeout}, cache: make(map[string]cachedResolution),
	}
}

func (r *OriginResolver) ResolveSource(ctx context.Context, pathName, _ string) (SourceResolution, error) {
	body, _ := json.Marshal(map[string]any{
		"streamPath": pathName, "region": r.region, "edgeNodeId": r.nodeID,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url, bytes.NewReader(body))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+r.token)
		var response *http.Response
		response, err = r.client.Do(req)
		if err == nil {
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				err = fmt.Errorf("origin resolver returned status %d", response.StatusCode)
			} else {
				var decoded struct {
					WHEPURL     string    `json:"whepUrl"`
					BearerToken string    `json:"bearerToken"`
					ExpiresAt   time.Time `json:"expiresAt"`
				}
				err = json.NewDecoder(response.Body).Decode(&decoded)
				if err == nil {
					u, parseErr := url.Parse(decoded.WHEPURL)
					if parseErr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || decoded.BearerToken == "" || !decoded.ExpiresAt.After(time.Now()) {
						err = fmt.Errorf("origin resolver returned an invalid response")
					} else {
						if u.Scheme == "http" {
							u.Scheme = "whep"
						} else {
							u.Scheme = "wheps"
						}
						resolved := SourceResolution{Source: u.String(), WHEPBearerToken: decoded.BearerToken}
						r.mu.Lock()
						r.cache[pathName] = cachedResolution{SourceResolution: resolved, expiresAt: decoded.ExpiresAt}
						r.mu.Unlock()
						return resolved, nil
					}
				}
			}
		}
	}

	r.mu.Lock()
	cached, ok := r.cache[pathName]
	r.mu.Unlock()
	if ok && cached.expiresAt.After(time.Now()) {
		return cached.SourceResolution, nil
	}
	if err == nil {
		err = fmt.Errorf("origin resolution failed")
	}
	return SourceResolution{}, fmt.Errorf("origin resolution failed: %s", strings.TrimSpace(err.Error()))
}
