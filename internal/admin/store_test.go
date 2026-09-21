package admin

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// newTestStore opens an ephemeral in-memory Store: OpenStore creates the
// schema and seeds the default {table, table, view} site_stream_configs row
// on an empty database (see seedDefaultSiteStreamConfigs), so "table"
// resolves to view "view" out of the box with no extra setup.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.db.Close()) })
	return s
}

// TestSiteStreamConfigsRoundTrips verifies SetSiteStreamConfigs persists
// rows and SiteStreamConfigs reads them back.
func TestSiteStreamConfigsRoundTrips(t *testing.T) {
	s := newTestStore(t)
	require.NoError(t, s.SetSiteStreamConfigs([]SiteStreamConfig{
		{SiteName: "site1", StreamName: "table1", ViewName: "fwh"},
	}))

	configs, err := s.SiteStreamConfigs()
	require.NoError(t, err)
	require.Len(t, configs, 1)
	require.Equal(t, "site1", configs[0].SiteName)
	require.Equal(t, "table1", configs[0].StreamName)
	require.Equal(t, "fwh", configs[0].ViewName)
}
