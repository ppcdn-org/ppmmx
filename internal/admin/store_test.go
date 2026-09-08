package admin

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// newTestStore opens an ephemeral in-memory Store: OpenStore creates the
// schema and seeds the default {table, table, view} site_stream_configs row
// on an empty database (see seedDefaultSiteStreamConfigs), so "table-view"
// is allowed out of the box with no extra setup.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.db.Close()) })
	return s
}

func TestIsStreamAllowedOutsideLivePrefixIsAlwaysAllowed(t *testing.T) {
	s := newTestStore(t)
	allowed, err := s.IsStreamAllowed("custom/anything")
	require.NoError(t, err)
	require.True(t, allowed)
}

func TestIsStreamAllowedPlainPathMatchesWhitelist(t *testing.T) {
	s := newTestStore(t)
	allowed, err := s.IsStreamAllowed("live/table-view")
	require.NoError(t, err)
	require.True(t, allowed)

	allowed, err = s.IsStreamAllowed("live/not-configured")
	require.NoError(t, err)
	require.False(t, allowed)
}

// TestIsStreamAllowedAcceptsCodecSuffixes covers the HEVC/H264 multitrack
// feature: a whitelisted stream must be allowed on both its /h264 and
// /hevc codec paths without any separate site_stream_configs row for them.
func TestIsStreamAllowedAcceptsCodecSuffixes(t *testing.T) {
	s := newTestStore(t)

	for _, path := range []string{
		"live/table-view/h264",
		"live/table-view/hevc",
	} {
		allowed, err := s.IsStreamAllowed(path)
		require.NoError(t, err)
		require.Truef(t, allowed, "path=%s", path)
	}

	// A codec-suffix-shaped path for a stream that isn't whitelisted at all
	// must still be rejected - stripping the suffix must not turn into an
	// accidental allow-everything.
	allowed, err := s.IsStreamAllowed("live/not-configured/hevc")
	require.NoError(t, err)
	require.False(t, allowed)

	// Anything other than exactly "/h264" or "/hevc" is not a recognized
	// codec segment and must not be stripped - it's compared as-is and
	// therefore rejected (no site_stream_configs row equals this literal
	// suffix).
	allowed, err = s.IsStreamAllowed("live/table-view/av1")
	require.NoError(t, err)
	require.False(t, allowed)
}
