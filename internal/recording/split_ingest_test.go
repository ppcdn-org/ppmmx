package recording

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/test"
)

// fakeIngestManager is a controllable IngestStarter for testing how
// SplitRecHandler drives ingest on round start/end.
type fakeIngestManager struct {
	mu        sync.Mutex
	startArgs []string
	stopArgs  []string
	// startFunc, if set, decides the (started, err) StartByPath returns and
	// can trigger side effects (e.g. simulating ffmpeg coming online).
	// Defaults to (true, nil) with no side effect.
	startFunc func(path string) (bool, error)
	// panicOnStart/panicOnStop assert a code path that must never trigger
	// ingest at all (e.g. round-end, or a path that was already live).
	panicOnStart bool
	panicOnStop  bool
}

func (f *fakeIngestManager) StartByPath(path string) (bool, error) {
	if f.panicOnStart {
		panic("StartByPath must not be called for this case: " + path)
	}
	f.mu.Lock()
	f.startArgs = append(f.startArgs, path)
	fn := f.startFunc
	f.mu.Unlock()
	if fn != nil {
		return fn(path)
	}
	return true, nil
}

func (f *fakeIngestManager) StopByPath(path string) {
	if f.panicOnStop {
		panic("StopByPath must not be called for this case: " + path)
	}
	f.mu.Lock()
	f.stopArgs = append(f.stopArgs, path)
	f.mu.Unlock()
}

func (f *fakeIngestManager) starts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.startArgs...)
}

func (f *fakeIngestManager) stops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.stopArgs...)
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "recordings.db"))
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	return NewManager(store)
}

func TestResolveController(t *testing.T) {
	const path = "live/test-ingest"

	t.Run("found directly never touches ingest", func(t *testing.T) {
		mgr := newTestManager(t)
		_, err := mgr.Start(path, Options{Format: "fmp4"}, &managerTestController{})
		require.NoError(t, err)

		h := NewSplitRecHandler(mgr, nil, test.NilLogger)
		h.SetIngestManager(&fakeIngestManager{panicOnStart: true, panicOnStop: true})

		ctrl, ok := h.resolveController(context.Background(), path, true, time.Second, 20*time.Millisecond)
		require.True(t, ok)
		require.NotNil(t, ctrl)
	})

	t.Run("round-end never starts ingest even when path is missing", func(t *testing.T) {
		mgr := newTestManager(t)
		h := NewSplitRecHandler(mgr, nil, test.NilLogger)
		h.SetIngestManager(&fakeIngestManager{panicOnStart: true})

		_, ok := h.resolveController(context.Background(), path, false, time.Second, 20*time.Millisecond)
		require.False(t, ok)
	})

	t.Run("no ingest manager configured returns immediately", func(t *testing.T) {
		mgr := newTestManager(t)
		h := NewSplitRecHandler(mgr, nil, test.NilLogger)
		// h.ingestMgr left nil.

		start := time.Now()
		_, ok := h.resolveController(context.Background(), path, true, 3*time.Second, 20*time.Millisecond)
		require.False(t, ok)
		require.Less(t, time.Since(start), time.Second, "must not wait when no ingest manager is wired in")
	})

	t.Run("unconfigured path fails fast instead of waiting out the timeout", func(t *testing.T) {
		mgr := newTestManager(t)
		h := NewSplitRecHandler(mgr, nil, test.NilLogger)
		fake := &fakeIngestManager{startFunc: func(string) (bool, error) {
			return false, fmt.Errorf("no ingest source configured")
		}}
		h.SetIngestManager(fake)

		start := time.Now()
		_, ok := h.resolveController(context.Background(), path, true, 3*time.Second, 20*time.Millisecond)
		require.False(t, ok)
		require.Less(t, time.Since(start), time.Second, "an unconfigured path shouldn't wait for the timeout")
		require.Equal(t, []string{path}, fake.starts())
	})

	t.Run("waits for ingest to bring the path online", func(t *testing.T) {
		mgr := newTestManager(t)
		h := NewSplitRecHandler(mgr, nil, test.NilLogger)
		fake := &fakeIngestManager{startFunc: func(p string) (bool, error) {
			go func() {
				time.Sleep(80 * time.Millisecond)
				_, _ = mgr.Start(p, Options{Format: "fmp4"}, &managerTestController{})
			}()
			return true, nil
		}}
		h.SetIngestManager(fake)

		ctrl, ok := h.resolveController(context.Background(), path, true, 3*time.Second, 20*time.Millisecond)
		require.True(t, ok)
		require.NotNil(t, ctrl)
		require.Equal(t, []string{path}, fake.starts())
	})

	t.Run("times out if ingest never comes online", func(t *testing.T) {
		mgr := newTestManager(t)
		h := NewSplitRecHandler(mgr, nil, test.NilLogger)
		h.SetIngestManager(&fakeIngestManager{}) // StartByPath succeeds but path never appears

		start := time.Now()
		_, ok := h.resolveController(context.Background(), path, true, 150*time.Millisecond, 20*time.Millisecond)
		require.False(t, ok)
		require.GreaterOrEqual(t, time.Since(start), 150*time.Millisecond)
	})

	t.Run("gives up when the request context is cancelled", func(t *testing.T) {
		mgr := newTestManager(t)
		h := NewSplitRecHandler(mgr, nil, test.NilLogger)
		h.SetIngestManager(&fakeIngestManager{})

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()

		start := time.Now()
		_, ok := h.resolveController(ctx, path, true, 10*time.Second, 20*time.Millisecond)
		require.False(t, ok)
		require.Less(t, time.Since(start), time.Second)
	})
}

// newSplitRecGinContext builds a bare gin.Context carrying a real
// *http.Request (for its Context()) - these tests call h.execute directly,
// bypassing ServeHTTP's auth checks, which are covered separately in
// split_auth_test.go.
func newSplitRecGinContext() *gin.Context {
	httpReq := httptest.NewRequest(http.MethodPost, "/api/split-rec", nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httpReq
	return ctx
}

func TestExecuteStartAndStopDriveIngest(t *testing.T) {
	const path = "live/table1-fwh"

	mgr := newTestManager(t)
	h := NewSplitRecHandler(mgr, nil, test.NilLogger)
	fake := &fakeIngestManager{startFunc: func(p string) (bool, error) {
		go func() {
			time.Sleep(30 * time.Millisecond)
			_, _ = mgr.Start(p, Options{Format: "fmp4"}, &managerTestController{})
		}()
		return true, nil
	}}
	h.SetIngestManager(fake)

	startReq := splitRecRequest{Time: "9999999999", Table: "table1", Game: "game1"}
	c := newSplitRecGinContext()
	require.NoError(t, h.execute(c, startReq))
	require.Equal(t, []string{path}, fake.starts(), "round start with no existing publisher must trigger ingest")
	require.Empty(t, fake.stops())

	stopReq := splitRecRequest{Time: "9999999999", Table: "table1", Game: "game1", GC: "round1"}
	c = newSplitRecGinContext()
	require.NoError(t, h.execute(c, stopReq))
	require.Equal(t, []string{path}, fake.stops(), "round end must stop ingest for the path")
}

func TestExecuteStopAlwaysCallsStopByPathEvenWithoutIngest(t *testing.T) {
	const path = "live/direct-publish"

	mgr := newTestManager(t)
	_, err := mgr.Start(path, Options{Format: "fmp4"}, &managerTestController{})
	require.NoError(t, err)

	h := NewSplitRecHandler(mgr, nil, test.NilLogger)
	fake := &fakeIngestManager{} // never asked to start anything - path was already live
	h.SetIngestManager(fake)

	startReq := splitRecRequest{Time: "9999999999", Table: "direct-publish", Game: "game1"}
	// tableToPath default is "live/<table>-fwh"; override via mapping so
	// this test's "direct-publish" table resolves to our pre-started path.
	SetTablePathMapping(map[string]string{"direct-publish": path})
	defer SetTablePathMapping(map[string]string{})

	c := newSplitRecGinContext()
	require.NoError(t, h.execute(c, startReq))
	require.Empty(t, fake.starts(), "path was already live, ingest must not be touched")

	stopReq := splitRecRequest{Time: "9999999999", Table: "direct-publish", Game: "game1", GC: "round1"}
	c = newSplitRecGinContext()
	require.NoError(t, h.execute(c, stopReq))
	require.Equal(t, []string{path}, fake.stops(), "StopByPath is called unconditionally on round end; it's a no-op if ingest wasn't involved")
}
