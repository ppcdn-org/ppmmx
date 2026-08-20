package admin

import (
	"database/sql"
	"fmt"
	"sync"

	_ "modernc.org/sqlite"
)

// Store persists admin settings in SQLite. admin_settings reads are served
// from an in-memory cache kept in sync by Set: these values (player
// domains, videostat env, admin credentials) are read on every
// /api/playUri request but change only when an admin edits them via the
// dashboard, and profiling showed the repeated SQLite round-trips
// dominating that hot path's CPU cost.
type Store struct {
	db *sql.DB
	mu sync.RWMutex

	cacheMu sync.RWMutex
	cache   map[string]string
}

func OpenStore(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath+"?_journal=WAL&_timeout=5000")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS admin_settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS site_stream_configs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		site_name TEXT NOT NULL,
		stream_name TEXT NOT NULL,
		view_name TEXT NOT NULL,
		UNIQUE(site_name, stream_name, view_name)
	)`); err != nil {
		db.Close()
		return nil, err
	}

	s := &Store{db: db, cache: make(map[string]string)}
	if err := s.loadCache(); err != nil {
		db.Close()
		return nil, fmt.Errorf("load admin_settings cache: %w", err)
	}
	return s, nil
}

func (s *Store) loadCache() error {
	rows, err := s.db.Query("SELECT key, value FROM admin_settings")
	if err != nil {
		return err
	}
	defer rows.Close()

	cache := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return err
		}
		cache[k] = v
	}
	if err := rows.Err(); err != nil {
		return err
	}

	s.cacheMu.Lock()
	s.cache = cache
	s.cacheMu.Unlock()
	return nil
}

func (s *Store) Close() { s.db.Close() }

// Get returns a setting's value from the in-memory cache (see the Store
// doc comment); it never touches SQLite.
func (s *Store) Get(key string) string {
	s.cacheMu.RLock()
	defer s.cacheMu.RUnlock()
	return s.cache[key]
}

func (s *Store) Set(key, value string) error {
	if _, err := s.db.Exec("INSERT OR REPLACE INTO admin_settings (key, value) VALUES (?, ?)", key, value); err != nil {
		return err
	}
	s.cacheMu.Lock()
	s.cache[key] = value
	s.cacheMu.Unlock()
	return nil
}

// ── Convenience ────────────────────────────────────────────────

func (s *Store) PlayerConfig(defaultMainDomain string) (mainDomain, backupDomain string) {
	mainDomain = s.Get("main_player_domain")
	backupDomain = s.Get("backup_player_domain")
	if mainDomain == "" {
		mainDomain = defaultMainDomain
	}
	if backupDomain == "" {
		backupDomain = "play.numericgame.ph"
	}
	return
}

func (s *Store) SetPlayerConfig(mainDomain, backupDomain string) error {
	if err := s.Set("main_player_domain", mainDomain); err != nil {
		return err
	}
	return s.Set("backup_player_domain", backupDomain)
}

var VideoStatEnvs = map[string]string{
	"test": "https://lotto-videostat.gelotto-test.com/api/stat",
	"uat":  "https://lotto-videostat.gelotto-uat.com/api/stat",
	"stag": "https://lotto-videostat.numericgame.io/api/stat",
	"prod": "https://lotto-videostat.numericgame.ph/api/stat",
}

func (s *Store) VideoStatEnv() string {
	env := s.Get("videostat_env")
	if env == "" {
		return "test"
	}
	return env
}

func (s *Store) SetVideoStatEnv(env string) error {
	if _, ok := VideoStatEnvs[env]; !ok {
		return fmt.Errorf("unknown videostat env: %s", env)
	}
	return s.Set("videostat_env", env)
}

func (s *Store) AdminCredentials() (string, string) {
	return s.Get("admin_username"), s.Get("admin_password_hash")
}

func (s *Store) SetAdminCredentials(username, passwordHash string) error {
	if err := s.Set("admin_username", username); err != nil {
		return err
	}
	return s.Set("admin_password_hash", passwordHash)
}

type SiteStreamConfig struct {
	ID         int64  `json:"id"`
	SiteName   string `json:"site_name"`
	StreamName string `json:"stream_name"`
	ViewName   string `json:"view_name"`
}

func (s *Store) SiteStreamConfigs() ([]SiteStreamConfig, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`SELECT id, site_name, stream_name, view_name
		FROM site_stream_configs ORDER BY site_name, stream_name, view_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var configs []SiteStreamConfig
	for rows.Next() {
		var config SiteStreamConfig
		if err := rows.Scan(&config.ID, &config.SiteName, &config.StreamName, &config.ViewName); err != nil {
			return nil, err
		}
		configs = append(configs, config)
	}
	return configs, rows.Err()
}

func (s *Store) SetSiteStreamConfigs(configs []SiteStreamConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM site_stream_configs"); err != nil {
		tx.Rollback()
		return err
	}
	for _, config := range configs {
		if _, err = tx.Exec(`INSERT INTO site_stream_configs (site_name, stream_name, view_name)
			VALUES (?, ?, ?)`, config.SiteName, config.StreamName, config.ViewName); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}
