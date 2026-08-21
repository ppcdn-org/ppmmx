package recordcleaner

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/test"
	"github.com/stretchr/testify/require"
)

func TestCleaner(t *testing.T) {
	timeNow = func() time.Time {
		return time.Date(2009, 5, 20, 22, 15, 25, 427000, time.Local)
	}

	dir := t.TempDir()

	const specialChars = "_-+*?^$()[]{}|"

	err := os.Mkdir(filepath.Join(dir, specialChars+"_mypath"), 0o755)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(dir, specialChars+"_mypath", "2008-05-20_22-15-25-000125.mp4"), []byte{1}, 0o644)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(dir, specialChars+"_mypath", "2009-05-20_22-15-25-000427.mp4"), []byte{1}, 0o644)
	require.NoError(t, err)

	c := &Cleaner{
		PathConfs: map[string]*conf.Path{
			"~^.*$": {
				Name:              "~^.*$",
				Regexp:            regexp.MustCompile("^.*$"),
				RecordPath:        filepath.Join(dir, specialChars+"_%path/%Y-%m-%d_%H-%M-%S-%f"),
				RecordFormat:      conf.RecordFormatFMP4,
				RecordDeleteAfter: conf.Duration(10 * time.Second),
			},
		},
		Parent: test.NilLogger,
	}
	c.Initialize()
	defer c.Close()

	time.Sleep(500 * time.Millisecond)

	_, err = os.Stat(filepath.Join(dir, specialChars+"_mypath", "2008-05-20_22-15-25-000125.mp4"))
	require.Error(t, err)

	_, err = os.Stat(filepath.Join(dir, specialChars+"_mypath", "2009-05-20_22-15-25-000427.mp4"))
	require.NoError(t, err)
}

func TestCleanerMultipleEntriesSamePath(t *testing.T) {
	timeNow = func() time.Time {
		return time.Date(2009, 5, 20, 22, 15, 25, 427000, time.Local)
	}

	dir := t.TempDir()

	err := os.Mkdir(filepath.Join(dir, "path1"), 0o755)
	require.NoError(t, err)

	err = os.Mkdir(filepath.Join(dir, "path2"), 0o755)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(dir, "path1", "2009-05-19_22-15-25-000427.mp4"), []byte{1}, 0o644)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(dir, "path2", "2009-05-19_22-15-25-000427.mp4"), []byte{1}, 0o644)
	require.NoError(t, err)

	c := &Cleaner{
		PathConfs: map[string]*conf.Path{
			"path1": {
				Name:              "path1",
				RecordPath:        filepath.Join(dir, "%path/%Y-%m-%d_%H-%M-%S-%f"),
				RecordFormat:      conf.RecordFormatFMP4,
				RecordDeleteAfter: conf.Duration(10 * time.Second),
			},
			"path2": {
				Name:              "path2",
				RecordPath:        filepath.Join(dir, "%path/%Y-%m-%d_%H-%M-%S-%f"),
				RecordFormat:      conf.RecordFormatFMP4,
				RecordDeleteAfter: conf.Duration(10 * 24 * time.Hour),
			},
		},
		Parent: test.NilLogger,
	}
	c.Initialize()
	defer c.Close()

	time.Sleep(500 * time.Millisecond)

	_, err = os.Stat(filepath.Join(dir, "path1", "2009-05-19_22-15-25-000427.mp4"))
	require.Error(t, err)

	_, err = os.Stat(filepath.Join(dir, "path1"))
	require.Error(t, err, "testing")

	_, err = os.Stat(filepath.Join(dir, "path2", "2009-05-19_22-15-25-000427.mp4"))
	require.NoError(t, err)
}

// TestCleanerMinFreeSpace verifies that when free space is below
// RecordMinFreeSpace, the cleaner force-deletes the globally oldest
// segments (oldest first, across all paths sharing the filesystem) until
// free space recovers, even though none of the segments have expired by
// age (RecordDeleteAfter is unset).
func TestCleanerMinFreeSpace(t *testing.T) {
	timeNow = func() time.Time {
		return time.Date(2009, 5, 20, 22, 15, 25, 427000, time.Local)
	}

	dir := t.TempDir()

	// %path comes after the common (non-%-containing) prefix, matching
	// production config (data/recordings/%Y%m%d/%path.%Y%m%d%H%M%S-%f):
	// CommonPath() for both paths resolves to the same root (dir itself),
	// so the cleaner treats them as sharing one filesystem, as intended.
	// path2 gets a second, even-older segment so its oldest one is a
	// valid deletion candidate distinct from "path2's latest" (which is
	// always protected - see TestCleanerMinFreeSpaceProtectsRecent).
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "path2.2009-05-19_19-00-00-000000.mp4"), make([]byte, 1000), 0o644))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "path1.2009-05-19_20-00-00-000000.mp4"), make([]byte, 1000), 0o644))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "path2.2009-05-19_21-00-00-000000.mp4"), make([]byte, 1000), 0o644))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "path1.2009-05-19_22-00-00-000000.mp4"), make([]byte, 1000), 0o644))

	// Simulate a filesystem that starts under the floor and recovers as
	// files are removed (the cleaner tracks reclaimed bytes itself via
	// os.Stat and only re-calls diskFreeBytes if that fails, so a single
	// low initial reading is enough to drive the whole reclaim loop).
	diskFreeBytes = func(string) (uint64, error) { return 500, nil }
	defer func() { diskFreeBytes = freeBytes }()

	c := &Cleaner{
		PathConfs: map[string]*conf.Path{
			"path1": {
				Name:         "path1",
				RecordPath:   filepath.Join(dir, "%path.%Y-%m-%d_%H-%M-%S-%f"),
				RecordFormat: conf.RecordFormatFMP4,
			},
			"path2": {
				Name:         "path2",
				RecordPath:   filepath.Join(dir, "%path.%Y-%m-%d_%H-%M-%S-%f"),
				RecordFormat: conf.RecordFormatFMP4,
			},
		},
		RecordMinFreeSpace: 2000, // needs 2 of the 4 (1000-byte) segments freed
		Parent:             test.NilLogger,
	}
	c.Initialize()
	defer c.Close()

	time.Sleep(500 * time.Millisecond)

	// Oldest two segments (path2@19:00, path1@20:00) must be gone...
	_, err := os.Stat(filepath.Join(dir, "path2.2009-05-19_19-00-00-000000.mp4"))
	require.Error(t, err)
	_, err = os.Stat(filepath.Join(dir, "path1.2009-05-19_20-00-00-000000.mp4"))
	require.Error(t, err)

	// ...but the newer two must survive, since deleting the two oldest
	// already reclaimed enough space.
	_, err = os.Stat(filepath.Join(dir, "path2.2009-05-19_21-00-00-000000.mp4"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir, "path1.2009-05-19_22-00-00-000000.mp4"))
	require.NoError(t, err)
}

// TestCleanerMinFreeSpaceProtectsRecent verifies that enforceMinFreeSpace
// never deletes a segment younger than minFreeSpaceProtectedAge, nor a
// path's single most recent segment (which may still be open/actively
// written to), even if that means free space stays under the configured
// minimum.
func TestCleanerMinFreeSpaceProtectsRecent(t *testing.T) {
	now := time.Date(2009, 5, 20, 22, 15, 25, 0, time.Local)
	timeNow = func() time.Time { return now }

	dir := t.TempDir()

	// Two segments for the only path: one old enough to be a normal
	// deletion candidate, and one that's both very old (so age alone
	// doesn't protect it) and this path's only/latest segment (so the
	// "don't delete the latest segment" rule must protect it instead).
	oldEnough := now.Add(-1 * time.Hour)
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "path1."+oldEnough.Format("2006-01-02_15-04-05")+"-000000.mp4"),
		make([]byte, 1000), 0o644))

	diskFreeBytes = func(string) (uint64, error) { return 0, nil } // always "critically low"
	defer func() { diskFreeBytes = freeBytes }()

	c := &Cleaner{
		PathConfs: map[string]*conf.Path{
			"path1": {
				Name:         "path1",
				RecordPath:   filepath.Join(dir, "%path.%Y-%m-%d_%H-%M-%S-%f"),
				RecordFormat: conf.RecordFormatFMP4,
			},
		},
		RecordMinFreeSpace: 1_000_000_000, // impossible to satisfy - forces the loop to run to completion
		Parent:             test.NilLogger,
	}
	c.Initialize()
	defer c.Close()

	time.Sleep(500 * time.Millisecond)

	// This is path1's only segment, so it's protected as "the latest"
	// even though it's an hour old and the disk claims to be at 0 bytes
	// free the entire time.
	_, err := os.Stat(filepath.Join(dir, "path1."+oldEnough.Format("2006-01-02_15-04-05")+"-000000.mp4"))
	require.NoError(t, err)
}

// TestCleanerMinFreeSpaceDisabledByDefault verifies that a zero
// RecordMinFreeSpace (the zero value, distinct from setDefaults' 8G
// default which only applies via conf.Load) never force-deletes
// unexpired segments.
func TestCleanerMinFreeSpaceDisabledByDefault(t *testing.T) {
	timeNow = func() time.Time {
		return time.Date(2009, 5, 20, 22, 15, 25, 427000, time.Local)
	}

	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "path1"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "path1", "2009-05-19_20-00-00-000000.mp4"), []byte{1}, 0o644))

	diskFreeBytes = func(string) (uint64, error) { return 0, nil } // "completely full"
	defer func() { diskFreeBytes = freeBytes }()

	c := &Cleaner{
		PathConfs: map[string]*conf.Path{
			"path1": {
				Name:         "path1",
				RecordPath:   filepath.Join(dir, "%path/%Y-%m-%d_%H-%M-%S-%f"),
				RecordFormat: conf.RecordFormatFMP4,
			},
		},
		// RecordMinFreeSpace intentionally left at zero.
		Parent: test.NilLogger,
	}
	c.Initialize()
	defer c.Close()

	time.Sleep(500 * time.Millisecond)

	_, err := os.Stat(filepath.Join(dir, "path1", "2009-05-19_20-00-00-000000.mp4"))
	require.NoError(t, err)
}
