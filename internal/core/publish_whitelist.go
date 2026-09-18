package core

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/mmxcontrol"
)

// publishWhitelistAppFetcher pulls the current set of apps ppcenter
// currently reports as eligible to publish. Satisfied by
// *mmxcontrol.AppSyncClient.
type publishWhitelistAppFetcher interface {
	Fetch(ctx context.Context) ([]mmxcontrol.AppCredential, error)
}

// tableViewPattern is the required shape for streamName once appId itself
// is valid: exactly one hyphen separating a tableId and a viewName
// segment, matching what recording.SplitRecHandler derives
// (appId+"/"+tableId+"-"+view - see docs/design/
// ppcdn-mmx-publish-whitelist.zh-CN.md). Each segment reuses split-rec's
// own identifier character set (recording.validIdentifier) so a name that
// would be rejected there is rejected here too, rather than accepted onto
// a path split-rec could never resolve back to a round.
var tableViewPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}-[A-Za-z0-9_-]{1,64}$`)

// codecSuffixes are the trailing path segments the HEVC/H264 multitrack
// WHIP convention appends after tableId-view (see internal/servers/webrtc/
// session.go's pathCodecFromName and docs/design/
// whip-hevc-h264-multitrack-simulcast-design.zh-CN.md:
// "/{appId}/{streamName}/{codecType}/whip"). Stripped before matching
// tableViewPattern below so a codec-suffixed publish isn't rejected as
// malformed - this check only cares about the appId/tableId-view shape,
// not which codec segment (if any) follows it.
var codecSuffixes = [...]string{"/h264", "/hevc"}

// appPublishWhitelist implements pathManagerStreamChecker (see
// path_manager.go) and recording.AppSecretLookup (see split.go). It
// replaces the old *admin.Store-backed site_stream_configs "live/"
// whitelist with a check on the appId/tableId-view namespace every
// publish path now lives under: pathName must be
// "<appId>/<tableId>-<viewName>", appId must be one ppcenter currently
// reports as eligible (see docs/design/ppcdn-mmx-publish-whitelist.zh-CN.md),
// and the remainder must match tableViewPattern. This applies globally -
// every publish attempt on this node, not just split-rec/recording paths -
// so a deployment with this wired in only ever accepts appId/tableId-view
// shaped stream names. The same cache also backs split-rec's own request
// signature verification (see recording.md §2.3): rather than a single
// deployment-wide shared secret, split-rec now verifies a request against
// the calling app's own appSecret, synced here alongside the publish
// whitelist itself.
type appPublishWhitelist struct {
	fetcher publishWhitelistAppFetcher
	parent  logger.Writer

	mu     sync.RWMutex
	apps   map[string]string // appId -> appSecret
	stopCh chan struct{}
	doneCh chan struct{}

	// onRevoked, if set, is called (outside the lock) for each appId that
	// disappears from a *successful* refresh, so the node can close that
	// app's live WHIP/WHEP sessions immediately rather than waiting for the
	// publisher/viewer to reconnect. AppIds absent from the previous snapshot
	// are never reported, so a cold start (empty cache -> first sync) cannot
	// cut anything. See docs/design/ppcdn-arrears-session-revocation.zh-CN.md.
	onRevoked func(appID string)
}

// SetRevocationHandler installs the whitelist-diff revocation callback. Call
// it once before start().
func (w *appPublishWhitelist) SetRevocationHandler(fn func(appID string)) {
	w.mu.Lock()
	w.onRevoked = fn
	w.mu.Unlock()
}

// newAppPublishWhitelist creates the whitelist but does not start polling -
// call start() once, typically right after construction in core.go.
func newAppPublishWhitelist(fetcher publishWhitelistAppFetcher, parent logger.Writer) *appPublishWhitelist {
	return &appPublishWhitelist{
		fetcher: fetcher,
		parent:  parent,
		apps:    make(map[string]string),
	}
}

// appSyncInterval is how often the whitelist re-pulls ppcenter's eligible
// appId list (see docs/design/ppcdn-mmx-publish-whitelist.zh-CN.md - 30s,
// balancing "how long a newly created/re-funded app waits before it can
// publish" against ppcenter request volume across a fleet of nodes).
const appSyncInterval = 30 * time.Second

// start launches the background poll loop. Does an immediate synchronous
// fetch first so the whitelist isn't empty (rejecting every publisher) for
// the first appSyncInterval after a fresh process start - a fetch failure
// here is only logged, not fatal, since the loop will retry on its own
// schedule and an empty cache degrades to "reject everything" rather than
// "allow everything", which is the safe direction to fail closed in.
func (w *appPublishWhitelist) start() {
	w.stopCh = make(chan struct{})
	w.doneCh = make(chan struct{})
	w.refresh()
	go w.run()
}

func (w *appPublishWhitelist) stop() {
	if w.stopCh == nil {
		return
	}
	close(w.stopCh)
	<-w.doneCh
}

func (w *appPublishWhitelist) run() {
	defer close(w.doneCh)
	ticker := time.NewTicker(appSyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			w.refresh()
		case <-w.stopCh:
			return
		}
	}
}

func (w *appPublishWhitelist) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	creds, err := w.fetcher.Fetch(ctx)
	if err != nil {
		if w.parent != nil {
			w.parent.Log(logger.Warn, "[publish-whitelist] app sync fetch failed: %v", err)
		}
		return
	}
	next := make(map[string]string, len(creds))
	for _, cred := range creds {
		next[cred.AppID] = cred.AppSecret
	}
	w.mu.Lock()
	previous := w.apps
	w.apps = next
	onRevoked := w.onRevoked
	w.mu.Unlock()

	if onRevoked == nil || len(previous) == 0 {
		return
	}
	for appID := range previous {
		if _, stillAllowed := next[appID]; !stillAllowed {
			onRevoked(appID)
		}
	}
}

func (w *appPublishWhitelist) appAllowed(appID string) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	_, ok := w.apps[appID]
	return ok
}

// AppSecret implements recording.AppSecretLookup: it returns the appSecret
// currently synced for appID, and whether that appId is known at all (a
// deleted/arrears app - or an appId that was never valid - reports
// ok=false, same as an appId that hasn't synced yet).
func (w *appPublishWhitelist) AppSecret(appID string) (string, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	secret, ok := w.apps[appID]
	return secret, ok
}

// IsStreamAllowed implements pathManagerStreamChecker.
func (w *appPublishWhitelist) IsStreamAllowed(pathName string) (bool, error) {
	appID, rest, ok := strings.Cut(pathName, "/")
	if !ok || appID == "" || rest == "" {
		return false, nil
	}
	if !w.appAllowed(appID) {
		return false, nil
	}
	for _, suffix := range codecSuffixes {
		rest = strings.TrimSuffix(rest, suffix)
	}
	return tableViewPattern.MatchString(rest), nil
}
