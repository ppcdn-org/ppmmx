package recording

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNowIsAlwaysShanghaiRegardlessOfSystemTimezone(t *testing.T) {
	// The recordings database must read the same way no matter which host
	// (dev machine, bare VPS, minimal container image) wrote a given row -
	// simulate a host whose system/process-local timezone is UTC (the
	// common container default) and confirm now() still reports +08:00.
	original := time.Local
	time.Local = time.UTC
	defer func() { time.Local = original }()

	got := now()
	_, offset := got.Zone()
	require.Equal(t, 8*60*60, offset, "now() must report UTC+8 (Asia/Shanghai) even when time.Local is UTC")
}

func TestStoreTimestampsRoundTripAsShanghai(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "recordings.db"))
	require.NoError(t, err)
	defer store.Close()

	rec := Record{
		ID:        "rec_tz_test",
		Path:      "live/test",
		Table:     "table1",
		Game:      "g1",
		Format:    "fmp4",
		Status:    "running",
		StartedAt: now(),
		FilePath:  "recording.mp4",
	}
	require.NoError(t, store.Insert(rec))

	require.NoError(t, store.CompleteRound(rec.ID, "gc1", "final.mp4", 1234, 60))

	got, err := store.Get(rec.ID)
	require.NoError(t, err)
	require.NotNil(t, got)

	_, startOffset := got.StartedAt.Zone()
	require.Equal(t, 8*60*60, startOffset, "started_at must round-trip as +08:00")
	_, stopOffset := got.StoppedAt.Zone()
	require.Equal(t, 8*60*60, stopOffset, "stopped_at must round-trip as +08:00")
}

func TestRecoverInterruptedWritesShanghaiStoppedAt(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "recordings.db"))
	require.NoError(t, err)
	defer store.Close()

	require.NoError(t, store.Insert(Record{
		ID: "rec_interrupted", Path: "live/test", Format: "fmp4",
		Status: "running", StartedAt: now(), FilePath: "recording.mp4",
	}))
	require.NoError(t, store.RecoverInterrupted())

	got, err := store.Get("rec_interrupted")
	require.NoError(t, err)
	require.Equal(t, "error", got.Status)
	_, offset := got.StoppedAt.Zone()
	require.Equal(t, 8*60*60, offset)
}

// TestRecordAppEnvRoundTrips verifies that AppEnv (the split-rec request's
// optional "app_env" field) is persisted and read back correctly, both when
// set and when left empty (callers that don't send app_env).
func TestRecordAppEnvRoundTrips(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "recordings.db"))
	require.NoError(t, err)
	defer store.Close()

	require.NoError(t, store.Insert(Record{
		ID: "rec_with_env", Path: "live/table1-fwh", Table: "table1", Game: "p2w001",
		AppEnv: "uat", Format: "fmp4", Status: "running", StartedAt: now(), FilePath: "recording.mp4",
	}))
	require.NoError(t, store.Insert(Record{
		ID: "rec_without_env", Path: "live/table1-fwv", Table: "table1", Game: "p2w002",
		Format: "fmp4", Status: "running", StartedAt: now(), FilePath: "recording.mp4",
	}))

	withEnv, err := store.Get("rec_with_env")
	require.NoError(t, err)
	require.Equal(t, "uat", withEnv.AppEnv)

	withoutEnv, err := store.Get("rec_without_env")
	require.NoError(t, err)
	require.Empty(t, withoutEnv.AppEnv)

	records, total, err := store.List("", "", 0, 0)
	require.NoError(t, err)
	require.Equal(t, 2, total)
	byID := map[string]Record{}
	for _, r := range records {
		byID[r.ID] = r
	}
	require.Equal(t, "uat", byID["rec_with_env"].AppEnv)
	require.Empty(t, byID["rec_without_env"].AppEnv)
}

// TestStoreMigratesAppEnvColumnOnExistingDatabase verifies that opening a
// database created before app_env was added (schema without that column)
// backfills the column instead of failing, so upgrading a deployment with
// an existing recordings.db doesn't break.
func TestStoreMigratesAppEnvColumnOnExistingDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "recordings.db")

	// Simulate the pre-app_env schema directly, bypassing OpenStore/migrate.
	preMigration, err := OpenStore(dbPath)
	require.NoError(t, err)
	_, err = preMigration.db.Exec(`ALTER TABLE recordings RENAME TO recordings_new`)
	require.NoError(t, err)
	_, err = preMigration.db.Exec(`
		CREATE TABLE recordings (
			id          TEXT PRIMARY KEY,
			path        TEXT NOT NULL,
			table_name  TEXT DEFAULT '',
			gc          TEXT DEFAULT '',
			game        TEXT DEFAULT '',
			format      TEXT NOT NULL DEFAULT 'fmp4',
			status      TEXT NOT NULL DEFAULT 'running',
			started_at  TEXT NOT NULL,
			stopped_at  TEXT,
			duration    INTEGER DEFAULT 0,
			file_size   INTEGER DEFAULT 0,
			file_path   TEXT NOT NULL
		)`)
	require.NoError(t, err)
	_, err = preMigration.db.Exec(`
		INSERT INTO recordings (id, path, table_name, game, format, status, started_at, file_path)
		VALUES ('rec_legacy', 'live/test', 'table1', 'p2w001', 'fmp4', 'completed', ?, 'legacy.mp4')`,
		now().Format(time.RFC3339))
	require.NoError(t, err)
	_, err = preMigration.db.Exec(`DROP TABLE recordings_new`)
	require.NoError(t, err)
	require.NoError(t, preMigration.Close())

	// Reopening must run migrate() again, adding app_env without erroring
	// and without losing the pre-existing row.
	reopened, err := OpenStore(dbPath)
	require.NoError(t, err)
	defer reopened.Close()

	got, err := reopened.Get("rec_legacy")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got.AppEnv, "pre-existing rows backfill app_env as empty string")

	// A fresh insert with AppEnv set must still work post-migration.
	require.NoError(t, reopened.Insert(Record{
		ID: "rec_after_migration", Path: "live/test2", AppEnv: "prod",
		Format: "fmp4", Status: "running", StartedAt: now(), FilePath: "new.mp4",
	}))
	got2, err := reopened.Get("rec_after_migration")
	require.NoError(t, err)
	require.Equal(t, "prod", got2.AppEnv)
}
