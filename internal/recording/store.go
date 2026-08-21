package recording

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
	_ "time/tzdata" // embed the IANA database so LoadLocation works even without an OS tzdata package
)

const maxRecords = 8000

// shanghaiLocation is the timezone every timestamp written to the
// recordings database is expressed in, independent of the host's own
// system timezone (dev machines, bare VPS installs, and minimal container
// images may all differ or default to UTC) - the audit trail must read the
// same way no matter which host wrote a given row.
var shanghaiLocation = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*60*60)
	}
	return loc
}()

// now returns the current time in shanghaiLocation, for any timestamp that
// ends up persisted to (or compared against) the recordings database.
func now() time.Time {
	return time.Now().In(shanghaiLocation)
}

// Record is one entry in the recording database.
type Record struct {
	ID        string    `json:"id"`
	Path      string    `json:"path"`
	Table     string    `json:"table"`
	GC        string    `json:"gc"`
	Game      string    `json:"game"`
	// AppEnv is the split-rec request's optional "app_env" field (see
	// ownerKey in split.go), persisted here purely for audit/traceability -
	// a deployment serving multiple game environments from one process can
	// tell which environment produced a given row. Empty for callers that
	// don't send app_env.
	AppEnv    string    `json:"appEnv,omitempty"`
	Format    string    `json:"format"`
	Status    string    `json:"status"` // running | completed | error
	StartedAt time.Time `json:"startedAt"`
	StoppedAt time.Time `json:"stoppedAt,omitempty"`
	Duration  int64     `json:"duration"`
	FileSize  int64     `json:"fileSize"`
	FilePath  string    `json:"filePath"`
}

// Store is a SQLite-based recording record store.
type Store struct {
	db *sql.DB
}

// OpenStore opens (or creates) the recording database at dbPath.
func OpenStore(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath+"?_journal=WAL&_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping database: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS recordings (
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
		);
		CREATE INDEX IF NOT EXISTS idx_rec_path ON recordings(path);
		CREATE INDEX IF NOT EXISTS idx_rec_status ON recordings(status);
		CREATE INDEX IF NOT EXISTS idx_rec_started ON recordings(started_at);
	`)
	if err != nil {
		return err
	}

	// app_env was added after recordings first shipped; existing databases
	// need this column backfilled. SQLite has no "ADD COLUMN IF NOT
	// EXISTS", so check pragma_table_info first - re-running ADD COLUMN on
	// a column that already exists is an error, not a no-op.
	var hasAppEnv bool
	row := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('recordings') WHERE name = 'app_env'`)
	var count int
	if err := row.Scan(&count); err != nil {
		return fmt.Errorf("check app_env column: %w", err)
	}
	hasAppEnv = count > 0
	if !hasAppEnv {
		if _, err := s.db.Exec(`ALTER TABLE recordings ADD COLUMN app_env TEXT DEFAULT ''`); err != nil {
			return fmt.Errorf("add app_env column: %w", err)
		}
	}

	return nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// RecoverInterrupted marks recordings left running by a previous process as failed.
func (s *Store) RecoverInterrupted() error {
	_, err := s.db.Exec("UPDATE recordings SET status='error', stopped_at=? WHERE status='running'", now().Format(time.RFC3339))
	return err
}

// Insert adds a new recording record. If total count exceeds maxRecords,
// the oldest completed/error records are evicted.
func (s *Store) Insert(r Record) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	_, err = tx.Exec(`
		INSERT OR REPLACE INTO recordings
			(id, path, table_name, gc, game, app_env, format, status, started_at, stopped_at, duration, file_size, file_path)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Path, r.Table, r.GC, r.Game, r.AppEnv, r.Format, r.Status,
		r.StartedAt.Format(time.RFC3339),
		nullTime(&r.StoppedAt),
		r.Duration, r.FileSize, r.FilePath)
	if err != nil {
		return fmt.Errorf("insert: %w", err)
	}

	// Evict excess records (keep last maxRecords)
	var count int
	if err := tx.QueryRow("SELECT COUNT(*) FROM recordings").Scan(&count); err != nil {
		return fmt.Errorf("count: %w", err)
	}
	if count > maxRecords {
		excess := count - maxRecords
		_, err = tx.Exec(`
			DELETE FROM recordings WHERE id IN (
				SELECT id FROM recordings
				WHERE status != 'running'
				ORDER BY started_at ASC
				LIMIT ?
			)`, excess)
		if err != nil {
			return fmt.Errorf("evict: %w", err)
		}
	}

	return tx.Commit()
}

// Update updates a recording's status and file metadata.
func (s *Store) Update(id string, status string, fileSize int64, duration int64) error {
	_, err := s.db.Exec(`
		UPDATE recordings SET status=?, stopped_at=?, file_size=?, duration=?
		WHERE id=?`,
		status, now().Format(time.RFC3339), fileSize, duration, id)
	return err
}

// CompleteRound finalizes a split-rec round: fills in the gc that was only
// known at stop time, the final (renamed) file path, its size, and marks
// the record completed.
func (s *Store) CompleteRound(id, gc, filePath string, fileSize, duration int64) error {
	_, err := s.db.Exec(`
		UPDATE recordings SET status='completed', stopped_at=?, gc=?, file_path=?, file_size=?, duration=?
		WHERE id=?`,
		now().Format(time.RFC3339), gc, filePath, fileSize, duration, id)
	return err
}

// Get returns a single record by ID.
func (s *Store) Get(id string) (*Record, error) {
	row := s.db.QueryRow("SELECT id,path,table_name,gc,game,app_env,format,status,started_at,stopped_at,duration,file_size,file_path FROM recordings WHERE id=?", id)
	return scanRecord(row)
}

// FindRunning returns the running recording for a path (if any).
func (s *Store) FindRunning(path string) (*Record, error) {
	row := s.db.QueryRow("SELECT id,path,table_name,gc,game,app_env,format,status,started_at,stopped_at,duration,file_size,file_path FROM recordings WHERE path=? AND status='running'", path)
	return scanRecord(row)
}

// List returns recordings matching the optional filters.
func (s *Store) List(path, status string, limit, offset int) ([]Record, int, error) {
	args := []any{}
	where := ""
	if path != "" {
		where = "WHERE path = ?"
		args = append(args, path)
	}
	if status != "" {
		if where == "" {
			where = "WHERE status = ?"
		} else {
			where += " AND status = ?"
		}
		args = append(args, status)
	}

	var total int
	countSQL := "SELECT COUNT(*) FROM recordings " + where
	if err := s.db.QueryRow(countSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	querySQL := "SELECT id,path,table_name,gc,game,app_env,format,status,started_at,stopped_at,duration,file_size,file_path FROM recordings " + where + " ORDER BY started_at DESC"
	if limit > 0 {
		querySQL += " LIMIT ?"
		args = append(args, limit)
	}
	if offset > 0 {
		querySQL += " OFFSET ?"
		args = append(args, offset)
	}

	rows, err := s.db.Query(querySQL, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var records []Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, 0, err
		}
		records = append(records, *r)
	}
	return records, total, nil
}

// Delete removes a record and its file from the database.
func (s *Store) Delete(id string) error {
	_, err := s.db.Exec("DELETE FROM recordings WHERE id=?", id)
	return err
}

func nullTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.Format(time.RFC3339)
}

func scanRecord(scanner interface {
	Scan(dest ...any) error
}) (*Record, error) {
	var r Record
	var started, stopped sql.NullString
	err := scanner.Scan(&r.ID, &r.Path, &r.Table, &r.GC, &r.Game, &r.AppEnv,
		&r.Format, &r.Status, &started, &stopped,
		&r.Duration, &r.FileSize, &r.FilePath)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if t, err := time.Parse(time.RFC3339, started.String); err == nil {
		r.StartedAt = t.In(shanghaiLocation)
	}
	if t, err := time.Parse(time.RFC3339, stopped.String); err == nil {
		r.StoppedAt = t.In(shanghaiLocation)
	}
	return &r, nil
}
