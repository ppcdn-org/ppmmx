package conf

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPatchIngestSourcesReplacesExistingLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.yml")
	require.NoError(t, os.WriteFile(path, []byte(
		"logLevel: info\n"+
			"ingestThreadEnable: true\n"+
			"ingestSources: [\"tencent:rtmp://old\"]\n"+
			"paths:\n"+
			"  all:\n"+
			"    source: publisher\n"), 0o644))

	require.NoError(t, PatchIngestSources(path, []string{"tencent:rtmp://new-a", "tencent:rtmp://new-b"}))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t,
		"logLevel: info\n"+
			"ingestThreadEnable: true\n"+
			"ingestSources: [\"tencent:rtmp://new-a\", \"tencent:rtmp://new-b\"]\n"+
			"paths:\n"+
			"  all:\n"+
			"    source: publisher\n", string(got))
}

func TestPatchIngestSourcesInsertsBeforePathsWhenMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.yml")
	require.NoError(t, os.WriteFile(path, []byte(
		"logLevel: info\n"+
			"paths:\n"+
			"  all:\n"+
			"    source: publisher\n"), 0o644))

	require.NoError(t, PatchIngestSources(path, []string{"tencent:rtmp://a"}))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t,
		"logLevel: info\n"+
			"ingestSources: [\"tencent:rtmp://a\"]\n"+
			"paths:\n"+
			"  all:\n"+
			"    source: publisher\n", string(got))
}

func TestPatchIngestSourcesEmptyList(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.yml")
	require.NoError(t, os.WriteFile(path, []byte("ingestSources: [\"a\"]\n"), 0o644))

	require.NoError(t, PatchIngestSources(path, nil))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "ingestSources: []\n", string(got))
}

func TestReadIngestSourcesRoundTripsWithPatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.yml")
	require.NoError(t, os.WriteFile(path, []byte("logLevel: info\npaths:\n  all:\n    source: publisher\n"), 0o644))

	require.NoError(t, PatchIngestSources(path, []string{"tencent:rtmp://a", "tencent:rtmp://b"}))

	got, err := ReadIngestSources(path)
	require.NoError(t, err)
	require.Equal(t, []string{"tencent:rtmp://a", "tencent:rtmp://b"}, got)
}

func TestReadIngestSourcesMissingKeyReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.yml")
	require.NoError(t, os.WriteFile(path, []byte("logLevel: info\n"), 0o644))

	got, err := ReadIngestSources(path)
	require.NoError(t, err)
	require.Equal(t, []string{}, got)
}
