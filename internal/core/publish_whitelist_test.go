package core

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/mmxcontrol"
	"github.com/bluenviron/mediamtx/internal/test"
)

var errFakeFetch = errors.New("fake fetch error")

// fakeAppFetcher is a controllable publishWhitelistAppFetcher for tests.
type fakeAppFetcher struct {
	appIDs []string
	err    error
	calls  int
}

func (f *fakeAppFetcher) Fetch(context.Context) ([]mmxcontrol.AppCredential, error) {
	f.calls++
	creds := make([]mmxcontrol.AppCredential, len(f.appIDs))
	for i, id := range f.appIDs {
		creds[i] = mmxcontrol.AppCredential{AppID: id, AppSecret: id + "-secret"}
	}
	return creds, f.err
}

func newTestWhitelist(fetcher publishWhitelistAppFetcher) *appPublishWhitelist {
	return newAppPublishWhitelist(fetcher, test.NilLogger)
}

func TestAppPublishWhitelistAllowsValidAppAndTableViewFormat(t *testing.T) {
	w := newTestWhitelist(&fakeAppFetcher{appIDs: []string{"app1"}})
	w.refresh()

	allowed, err := w.IsStreamAllowed("app1/table-view")
	require.NoError(t, err)
	require.True(t, allowed)
}

func TestAppPublishWhitelistRejectsUnknownApp(t *testing.T) {
	w := newTestWhitelist(&fakeAppFetcher{appIDs: []string{"app1"}})
	w.refresh()

	allowed, err := w.IsStreamAllowed("app2/table-view")
	require.NoError(t, err)
	require.False(t, allowed)
}

func TestAppPublishWhitelistRejectsMalformedStreamName(t *testing.T) {
	w := newTestWhitelist(&fakeAppFetcher{appIDs: []string{"app1"}})
	w.refresh()

	for _, path := range []string{
		"app1/notablehyphen", // no hyphen at all: not tableId-view shaped
		"app1/table-",        // empty view segment
		"app1/-view",         // empty table segment
		"app1/",              // empty streamName entirely
		"app1",               // no "/" separator at all
		"",
	} {
		allowed, err := w.IsStreamAllowed(path)
		require.NoError(t, err)
		require.Falsef(t, allowed, "path=%q must be rejected", path)
	}
}

// TestAppPublishWhitelistAllowsMultipleHyphensInEitherSegment covers that
// tableId and viewName may themselves contain hyphens (they reuse
// split-rec's own identifier character set - see recording.validIdentifier
// / identifierPattern), so a streamName with more than one hyphen is not
// automatically malformed - only "no hyphen at all" or an empty segment is.
func TestAppPublishWhitelistAllowsMultipleHyphensInEitherSegment(t *testing.T) {
	w := newTestWhitelist(&fakeAppFetcher{appIDs: []string{"app1"}})
	w.refresh()

	allowed, err := w.IsStreamAllowed("app1/too-many-hyphens")
	require.NoError(t, err)
	require.True(t, allowed)
}

// TestAppPublishWhitelistAllowsCodecSuffixedPath covers the HEVC/H264
// multitrack WHIP convention ("/{appId}/{streamName}/{codecType}/whip" -
// see internal/servers/webrtc/session.go's pathCodecFromName): a publish
// path ending in "/h264" or "/hevc" after the tableId-view segment must
// still be accepted, not rejected as a malformed 3-segment path.
func TestAppPublishWhitelistAllowsCodecSuffixedPath(t *testing.T) {
	w := newTestWhitelist(&fakeAppFetcher{appIDs: []string{"app1"}})
	w.refresh()

	for _, path := range []string{"app1/table-view/h264", "app1/table-view/hevc"} {
		allowed, err := w.IsStreamAllowed(path)
		require.NoError(t, err)
		require.Truef(t, allowed, "path=%q must be allowed", path)
	}
}

// TestAppPublishWhitelistRejectsUnknownCodecSuffix covers that only the
// two recognized codec segments are stripped - any other trailing segment
// is still a malformed (extra-segment) path and must be rejected.
func TestAppPublishWhitelistRejectsUnknownCodecSuffix(t *testing.T) {
	w := newTestWhitelist(&fakeAppFetcher{appIDs: []string{"app1"}})
	w.refresh()

	allowed, err := w.IsStreamAllowed("app1/table-view/av1")
	require.NoError(t, err)
	require.False(t, allowed)
}

func TestAppPublishWhitelistEmptyCacheRejectsEverything(t *testing.T) {
	// A whitelist that has never successfully synced (or has an empty
	// eligible-app list) must fail closed, not open - see the doc comment
	// on start().
	w := newTestWhitelist(&fakeAppFetcher{appIDs: nil})
	w.refresh()

	allowed, err := w.IsStreamAllowed("app1/table-view")
	require.NoError(t, err)
	require.False(t, allowed)
}

func TestAppPublishWhitelistRefreshFailureKeepsPreviousCache(t *testing.T) {
	fetcher := &fakeAppFetcher{appIDs: []string{"app1"}}
	w := newTestWhitelist(fetcher)
	w.refresh()

	allowed, err := w.IsStreamAllowed("app1/table-view")
	require.NoError(t, err)
	require.True(t, allowed)

	// A transient fetch failure must not wipe out the last-known-good set.
	fetcher.err = errFakeFetch
	w.refresh()

	allowed, err = w.IsStreamAllowed("app1/table-view")
	require.NoError(t, err)
	require.True(t, allowed, "a failed refresh must not clear the existing cache")
}

func TestAppPublishWhitelistRefreshReplacesCacheWholesale(t *testing.T) {
	fetcher := &fakeAppFetcher{appIDs: []string{"app1"}}
	w := newTestWhitelist(fetcher)
	w.refresh()

	allowed, _ := w.IsStreamAllowed("app1/table-view")
	require.True(t, allowed)

	// app1 stops appearing (deleted/arrears) and app2 is now eligible: the
	// next refresh must drop app1 and pick up app2, not merge the two sets.
	fetcher.appIDs = []string{"app2"}
	w.refresh()

	allowed, _ = w.IsStreamAllowed("app1/table-view")
	require.False(t, allowed, "app1 must lose access once it stops appearing in a sync")
	allowed, _ = w.IsStreamAllowed("app2/table-view")
	require.True(t, allowed)
}

func TestAppPublishWhitelistStartStopIsIdempotentAndSafe(t *testing.T) {
	w := newTestWhitelist(&fakeAppFetcher{appIDs: []string{"app1"}})
	w.start()
	w.stop()
}

// TestAppPublishWhitelistAppSecret covers the same cache also backing
// split-rec's own signature verification (see recording.AppSecretLookup).
func TestAppPublishWhitelistAppSecret(t *testing.T) {
	w := newTestWhitelist(&fakeAppFetcher{appIDs: []string{"app1"}})
	w.refresh()

	secret, ok := w.AppSecret("app1")
	require.True(t, ok)
	require.Equal(t, "app1-secret", secret)

	_, ok = w.AppSecret("unknown-app")
	require.False(t, ok)
}
