package ingest

import (
	"crypto/md5" //nolint:gosec // test asserts against the same algorithm buildPullURL uses
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLastPathSegment(t *testing.T) {
	for _, ca := range []struct {
		name string
		in   string
		out  string
	}{
		{
			name: "plain rtmp",
			in:   "rtmp://cdn.example.com/live/demo-stream",
			out:  "demo-stream",
		},
		{
			name: "with query string",
			in:   "rtmp://cdn.example.com/live/demo-stream?txSecret=abc&txTime=123",
			out:  "demo-stream",
		},
		{
			name: "http flv with fragment",
			in:   "http://cdn.example.com/live/table1-fwv.flv#frag",
			out:  "table1-fwv.flv",
		},
		{
			name: "trailing slash",
			in:   "rtmp://cdn.example.com/live/demo-stream/",
			out:  "demo-stream",
		},
		{
			name: "no path",
			in:   "rtmp://cdn.example.com",
			out:  "",
		},
		{
			name: "empty",
			in:   "",
			out:  "",
		},
	} {
		t.Run(ca.name, func(t *testing.T) {
			require.Equal(t, ca.out, lastPathSegment(ca.in))
		})
	}
}

func TestManagerInitializeSkipsInvalidSources(t *testing.T) {
	m := &Manager{
		Sources: []string{
			"",
			"   ",
			"no-colon-no-cdn-name",
			"other:",                       // empty url after the colon
			"other:rtmp://cdn.example.com", // no path segment to derive a name from
			"other:rtmp://cdn.example.com/live/valid-stream",
		},
		LocalRTMPURL: "rtmp://127.0.0.1:1935",
	}
	m.Initialize()

	// Initialize only parses Sources into a lookup - it must not start
	// anything on its own (see TestStartByPath for the on-demand trigger).
	require.Empty(t, m.workers)

	require.Len(t, m.bySource, 1)
	cfg, ok := m.bySource["live/valid-stream"]
	require.True(t, ok)
	require.Equal(t, "other", cfg.cdnName)
	require.Equal(t, "rtmp://cdn.example.com/live/valid-stream", cfg.rawURL)
}

// unreachable/nonexistent source: ffmpeg (or its absence) will fail
// immediately, exercising the retry/backoff path's ctx-cancellation
// awareness rather than a real long-running pull.
const unreachableSource = "other:rtmp://127.0.0.1:1/live/nonexistent-stream"

func TestStartByPath(t *testing.T) {
	t.Run("starts a configured, not-yet-running path", func(t *testing.T) {
		m := &Manager{Sources: []string{unreachableSource}, LocalRTMPURL: "rtmp://127.0.0.1:1935"}
		m.Initialize()
		defer m.Close()

		started, err := m.StartByPath("live/nonexistent-stream")
		require.NoError(t, err)
		require.True(t, started)
		require.Contains(t, m.workers, "live/nonexistent-stream")
	})

	t.Run("second call for the same path is a no-op, not an error", func(t *testing.T) {
		m := &Manager{Sources: []string{unreachableSource}, LocalRTMPURL: "rtmp://127.0.0.1:1935"}
		m.Initialize()
		defer m.Close()

		_, err := m.StartByPath("live/nonexistent-stream")
		require.NoError(t, err)

		started, err := m.StartByPath("live/nonexistent-stream")
		require.NoError(t, err)
		require.False(t, started)
	})

	t.Run("unconfigured path returns an error and starts nothing", func(t *testing.T) {
		m := &Manager{Sources: []string{unreachableSource}, LocalRTMPURL: "rtmp://127.0.0.1:1935"}
		m.Initialize()
		defer m.Close()

		started, err := m.StartByPath("live/never-configured")
		require.Error(t, err)
		require.False(t, started)
		require.NotContains(t, m.workers, "live/never-configured")
	})
}

func TestStopByPath(t *testing.T) {
	t.Run("stops a running worker and frees the path for a fresh start", func(t *testing.T) {
		m := &Manager{Sources: []string{unreachableSource}, LocalRTMPURL: "rtmp://127.0.0.1:1935"}
		m.Initialize()
		defer m.Close()

		_, err := m.StartByPath("live/nonexistent-stream")
		require.NoError(t, err)

		m.StopByPath("live/nonexistent-stream")
		require.NotContains(t, m.workers, "live/nonexistent-stream")

		// the slot must be free immediately, not just eventually once the
		// old worker's goroutine finishes unwinding.
		started, err := m.StartByPath("live/nonexistent-stream")
		require.NoError(t, err)
		require.True(t, started, "StartByPath should be able to reuse the path right after StopByPath")
	})

	t.Run("no-op for a path with no running worker", func(t *testing.T) {
		m := &Manager{Sources: []string{unreachableSource}, LocalRTMPURL: "rtmp://127.0.0.1:1935"}
		m.Initialize()
		defer m.Close()

		require.NotPanics(t, func() { m.StopByPath("live/nonexistent-stream") })
		require.NotPanics(t, func() { m.StopByPath("live/never-configured") })
	})
}

func TestManagerCloseStopsWorkers(t *testing.T) {
	m := &Manager{
		Sources:      []string{unreachableSource},
		LocalRTMPURL: "rtmp://127.0.0.1:1935",
	}
	m.Initialize()
	started, err := m.StartByPath("live/nonexistent-stream")
	require.NoError(t, err)
	require.True(t, started)

	done := make(chan struct{})
	go func() {
		m.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close() did not return in time; worker is not honoring context cancellation")
	}
}

func TestWorkerBuildPullURL(t *testing.T) {
	t.Run("non-tencent CDN is passed through unchanged", func(t *testing.T) {
		w := newWorker("other", "rtmp://cdn.example.com/live/foo?already=signed", "rtmp://127.0.0.1:1935/live/foo", "", nil)
		require.Equal(t, "rtmp://cdn.example.com/live/foo?already=signed", w.buildPullURL())
	})

	t.Run("tencent CDN gets a fresh txTime/txSecret appended", func(t *testing.T) {
		w := newWorker(cdnNameTencent, "rtmp://play.example.com/live/demo-stream", "rtmp://127.0.0.1:1935/live/demo-stream",
			"test-secret-key-back", nil)

		before := time.Now()
		signed := w.buildPullURL()
		after := time.Now()

		require.Contains(t, signed, "rtmp://play.example.com/live/demo-stream?txTime=")
		require.Contains(t, signed, "&txSecret=")

		txTime, txSecret := parseTencentQuery(t, signed)
		expiry, err := strconv.ParseInt(txTime, 16, 64)
		require.NoError(t, err)
		gotExpiry := time.Unix(expiry, 0)
		require.WithinRange(t, gotExpiry, before.Add(tencentSignatureTTL-time.Second), after.Add(tencentSignatureTTL+time.Second))

		wantSecret := fmt.Sprintf("%x", md5.Sum([]byte("test-secret-key-back"+"demo-stream"+txTime))) //nolint:gosec
		require.Equal(t, wantSecret, txSecret)
	})

	t.Run("tencent CDN re-signs on every call, not just the first", func(t *testing.T) {
		w := newWorker(cdnNameTencent, "rtmp://play.example.com/live/demo-stream", "rtmp://127.0.0.1:1935/live/demo-stream",
			"test-secret-key-back", nil)

		first := w.buildPullURL()
		time.Sleep(1100 * time.Millisecond)
		second := w.buildPullURL()

		require.NotEqual(t, first, second, "txTime should advance between calls a second apart")
	})

	t.Run("existing query string uses & instead of ?", func(t *testing.T) {
		w := newWorker(cdnNameTencent, "rtmp://play.example.com/live/demo-stream?foo=bar", "rtmp://127.0.0.1:1935/live/demo-stream",
			"test-secret-key-back", nil)
		require.Contains(t, w.buildPullURL(), "?foo=bar&txTime=")
	})
}

// parseTencentQuery extracts txTime and txSecret from a signed pull URL
// built by buildPullURL, without pulling in net/url (query values here are
// hex/hash strings with no special characters to escape).
func parseTencentQuery(t *testing.T, signedURL string) (txTime, txSecret string) {
	t.Helper()
	i := strings.Index(signedURL, "txTime=")
	require.GreaterOrEqual(t, i, 0)
	rest := signedURL[i+len("txTime="):]
	j := strings.Index(rest, "&txSecret=")
	require.GreaterOrEqual(t, j, 0)
	return rest[:j], rest[j+len("&txSecret="):]
}
