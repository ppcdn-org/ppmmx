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

// TestViewsForTableDefaultSeed covers the seeded {table, table, view} row.
func TestViewsForTableDefaultSeed(t *testing.T) {
	s := newTestStore(t)
	views, err := s.ViewsForTable("table")
	require.NoError(t, err)
	require.Equal(t, []string{"view"}, views)
}

// TestViewsForTableMultipleViews covers a table with several configured
// views, all returned together for split-rec's fan-out.
func TestViewsForTableMultipleViews(t *testing.T) {
	s := newTestStore(t)
	require.NoError(t, s.SetSiteStreamConfigs([]SiteStreamConfig{
		{SiteName: "table", StreamName: "table1", ViewName: "fwh"},
		{SiteName: "table", StreamName: "table1", ViewName: "fwv"},
	}))

	views, err := s.ViewsForTable("table1")
	require.NoError(t, err)
	require.Equal(t, []string{"fwh", "fwv"}, views)
}

// TestViewsForTableUnknownTableReturnsEmpty covers a table with no
// configured rows at all.
func TestViewsForTableUnknownTableReturnsEmpty(t *testing.T) {
	s := newTestStore(t)
	views, err := s.ViewsForTable("not-configured")
	require.NoError(t, err)
	require.Empty(t, views)
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
