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
	w.apps = next
	w.mu.Unlock()
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
	return tableViewPattern.MatchString(rest), nil
}
