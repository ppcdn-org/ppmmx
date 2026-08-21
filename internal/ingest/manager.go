// Package ingest pulls external CDN sources with ffmpeg and republishes them
// to mmx's own local RTMP listener, so they get recorded/distributed like any
// other publisher. See docs/cdn-ingest-thread.md.
package ingest

import (
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/bluenviron/mediamtx/internal/logger"
)

// sourceConfig is a parsed Sources entry, keyed by the local path it
// republishes to (see Initialize).
type sourceConfig struct {
	cdnName string
	rawURL  string
}

// Manager resolves configured CDN sources to local paths and starts/stops an
// ffmpeg pull-and-republish worker per path on demand - not at boot. A path
// only starts pulling when something calls StartByPath for it (in practice,
// SplitRecHandler on a POST /api/split-rec round-start for a path with no
// publisher already live), and stops when StopByPath is called or the
// process shuts down.
type Manager struct {
	// Sources is the list of CDN sources to pull from, each formatted as
	// "{cdnName}:{rtmpUrl}", e.g.
	// "tencent:rtmp://play.example.com/live/demo-stream". rtmpUrl must
	// already be a complete, ready-to-pull URL (any auth query params a
	// non-tencent CDN requires must already be embedded in it - mmx only
	// signs for cdnName "tencent", see buildPullURL).
	Sources []string
	// LocalRTMPURL is mmx's own RTMP listener, e.g. "rtmp://127.0.0.1:1935".
	// Each source is republished to LocalRTMPURL + "/" + path, where path is
	// "live/<name>" and <name> is the last path segment of the source URL.
	LocalRTMPURL string
	// TencentSecretKeyBack signs "tencent:" sources (TX_SECRET_KEY_BACK,
	// the same key internal/admin uses for play.example.com play links).
	TencentSecretKeyBack string
	Parent               logger.Writer

	mu       sync.Mutex
	bySource map[string]sourceConfig // path -> config, populated once by Initialize
	workers  map[string]*worker      // path -> currently-running worker
	wg       sync.WaitGroup
}

// Initialize parses Sources into a path -> source lookup. It does not start
// any pulling; call StartByPath for that.
func (m *Manager) Initialize() {
	m.bySource = make(map[string]sourceConfig)
	m.workers = make(map[string]*worker)

	for _, entry := range m.Sources {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		cdnName, rawURL, ok := strings.Cut(entry, ":")
		if !ok || strings.TrimSpace(rawURL) == "" {
			m.log(logger.Warn, "ingest: skipping malformed source (want \"cdnName:rtmpUrl\"): %s", entry)
			continue
		}
		rawURL = strings.TrimSpace(rawURL)

		name := lastPathSegment(rawURL)
		if name == "" {
			m.log(logger.Warn, "ingest: skipping source, could not derive a stream name: %s", entry)
			continue
		}

		if cdnName == cdnNameTencent && m.TencentSecretKeyBack == "" {
			m.log(logger.Warn, "ingest: %q needs a tencent signature but TX_SECRET_KEY_BACK is not configured; "+
				"the pull will likely be rejected", entry)
		}

		m.bySource["live/"+name] = sourceConfig{cdnName: cdnName, rawURL: rawURL}
	}
}

// StartByPath starts pulling the source configured for path (e.g.
// "live/demo-stream"), if one is configured and isn't already running.
// started is true only if this call actually launched a new worker; a
// non-nil err means nothing is configured for path at all - the caller
// shouldn't bother waiting for it to come up.
func (m *Manager) StartByPath(path string) (started bool, err error) {
	m.mu.Lock()

	if _, running := m.workers[path]; running {
		m.mu.Unlock()
		return false, nil
	}

	cfg, ok := m.bySource[path]
	if !ok {
		m.mu.Unlock()
		return false, fmt.Errorf("no ingest source configured for %q", path)
	}

	w := newWorker(cfg.cdnName, cfg.rawURL, m.LocalRTMPURL+"/"+path, m.TencentSecretKeyBack, m.Parent)
	m.workers[path] = w
	m.mu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		w.run()

		// Only clear our own slot: if StopByPath+StartByPath already
		// replaced us with a newer worker for the same path while we were
		// still winding down, leave that one alone.
		m.mu.Lock()
		if m.workers[path] == w {
			delete(m.workers, path)
		}
		m.mu.Unlock()
	}()

	return true, nil
}

// StopByPath stops the worker (if any) currently pulling path. It's a
// no-op if nothing is running for that path - e.g. the stream came from a
// direct publisher, not ingest.
func (m *Manager) StopByPath(path string) {
	m.mu.Lock()
	w, ok := m.workers[path]
	if ok {
		// Free the slot immediately so a StartByPath racing right behind
		// this (e.g. a new round on the same path) isn't blocked on the old
		// worker's ffmpeg process actually exiting.
		delete(m.workers, path)
	}
	m.mu.Unlock()

	if ok {
		m.log(logger.Info, "ingest: stopping %s", path)
		w.cancel()
	}
}

// Close stops every currently-running worker and waits for their ffmpeg
// processes to exit.
func (m *Manager) Close() {
	m.mu.Lock()
	workers := make([]*worker, 0, len(m.workers))
	for _, w := range m.workers {
		workers = append(workers, w)
	}
	m.workers = map[string]*worker{}
	m.mu.Unlock()

	for _, w := range workers {
		w.cancel()
	}
	m.wg.Wait()
}

func (m *Manager) log(level logger.Level, format string, args ...any) {
	if m.Parent != nil {
		m.Parent.Log(level, format, args...)
	}
}

// lastPathSegment returns the final "/"-separated segment of a URL's path
// (query string and fragment excluded), e.g.
// "rtmp://cdn.example.com/live/foo-bar?txSecret=x" -> "foo-bar". Returns ""
// for a URL with no path beyond the host, e.g. "rtmp://cdn.example.com".
func lastPathSegment(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}

	p := strings.Trim(u.Path, "/")
	if p == "" {
		return ""
	}
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
