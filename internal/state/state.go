// Package state is the machine-local SQLite store: per-app paths and history,
// settings and the Drive token. Nothing here is synced.
package state

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver" // pure-Go SQLite (no C compiler)
	_ "github.com/ncruces/go-sqlite3/embed"
)

// AppState is what this machine knows about one app.
type AppState struct {
	ID           string
	InstallPath  string // empty = default <apps dir>/<id>
	LastLaunched time.Time
	Approved     string // the exact commands the user approved before first run
}

// Store wraps the database. Safe for concurrent use.
type Store struct {
	db *sql.DB
	mu sync.Mutex
}

const schema = `
CREATE TABLE IF NOT EXISTS app_state (
  id            TEXT PRIMARY KEY,
  install_path  TEXT NOT NULL DEFAULT '',
  last_launched INTEGER NOT NULL DEFAULT 0,
  approved      TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS sync_records (
  app_id    TEXT NOT NULL,
  path      TEXT NOT NULL,
  sha256    TEXT NOT NULL,
  remote_id TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (app_id, path)
);
CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
-- Older versions stored API keys here; the feature was removed.
DROP TABLE IF EXISTS api_keys;
`

// Open opens (and creates) state.db in dir.
func Open(dir string) (*Store, error) {
	// SQLite URI: "file:C:/Users/..." on Windows, "file:/home/..." elsewhere.
	// Only %, ? and # need escaping in the path part.
	p := filepath.ToSlash(filepath.Join(dir, "state.db"))
	p = strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(p)
	db, err := sql.Open("sqlite3", "file:"+p+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(wal)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// App returns the state for one app (zero value if none).
func (s *Store) App(id string) (AppState, error) {
	a := AppState{ID: id}
	var last int64
	err := s.db.QueryRow(`SELECT install_path, last_launched, approved FROM app_state WHERE id = ?`, id).
		Scan(&a.InstallPath, &last, &a.Approved)
	if errors.Is(err, sql.ErrNoRows) {
		return a, nil
	}
	if last > 0 {
		a.LastLaunched = time.Unix(last, 0)
	}
	return a, err
}

// Apps returns all app states keyed by id.
func (s *Store) Apps() (map[string]AppState, error) {
	rows, err := s.db.Query(`SELECT id, install_path, last_launched, approved FROM app_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]AppState{}
	for rows.Next() {
		var a AppState
		var last int64
		if err := rows.Scan(&a.ID, &a.InstallPath, &last, &a.Approved); err != nil {
			return nil, err
		}
		if last > 0 {
			a.LastLaunched = time.Unix(last, 0)
		}
		out[a.ID] = a
	}
	return out, rows.Err()
}

func (s *Store) ensure(id string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO app_state (id) VALUES (?)`, id)
	return err
}

func (s *Store) SetInstallPath(id, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensure(id); err != nil {
		return err
	}
	_, err := s.db.Exec(`UPDATE app_state SET install_path = ? WHERE id = ?`, path, id)
	return err
}

func (s *Store) SetLastLaunched(id string, t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensure(id); err != nil {
		return err
	}
	_, err := s.db.Exec(`UPDATE app_state SET last_launched = ? WHERE id = ?`, t.Unix(), id)
	return err
}

func (s *Store) SetApproved(id, commands string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensure(id); err != nil {
		return err
	}
	_, err := s.db.Exec(`UPDATE app_state SET approved = ? WHERE id = ?`, commands, id)
	return err
}

// Setting returns a setting value ("" if unset).
func (s *Store) Setting(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetSetting stores a setting; an empty value deletes it.
func (s *Store) SetSetting(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if value == "" {
		_, err := s.db.Exec(`DELETE FROM settings WHERE key = ?`, key)
		return err
	}
	_, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// SyncRecord is the version of one file at its last successful sync.
type SyncRecord struct {
	Path     string
	SHA256   string
	RemoteID string
}

// SyncRecords returns the last-synced versions of an app's files.
func (s *Store) SyncRecords(appID string) (map[string]SyncRecord, error) {
	rows, err := s.db.Query(`SELECT path, sha256, remote_id FROM sync_records WHERE app_id = ?`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]SyncRecord{}
	for rows.Next() {
		var r SyncRecord
		if err := rows.Scan(&r.Path, &r.SHA256, &r.RemoteID); err != nil {
			return nil, err
		}
		out[r.Path] = r
	}
	return out, rows.Err()
}

// PutSyncRecord stores the synced version of one file.
func (s *Store) PutSyncRecord(appID string, r SyncRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO sync_records (app_id, path, sha256, remote_id) VALUES (?, ?, ?, ?)
		ON CONFLICT(app_id, path) DO UPDATE SET sha256 = excluded.sha256, remote_id = excluded.remote_id`,
		appID, r.Path, r.SHA256, r.RemoteID)
	return err
}

// DeleteSyncRecord forgets one file's synced version (the file was deleted).
func (s *Store) DeleteSyncRecord(appID, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM sync_records WHERE app_id = ? AND path = ?`, appID, path)
	return err
}

// ClearSyncRecords forgets all of an app's synced versions, so the next sync
// compares both sides from scratch (used when the synced folder changes).
func (s *Store) ClearSyncRecords(appID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM sync_records WHERE app_id = ?`, appID)
	return err
}

// CheckpointSQLite merges a database's write-ahead log into the main file so
// it can be copied as one file. Only call it while no app has it open.
func CheckpointSQLite(path string) error {
	p := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(filepath.ToSlash(path))
	db, err := sql.Open("sqlite3", "file:"+p+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	var busy, logFrames, done int
	if err := db.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &done); err != nil {
		return err
	}
	if busy != 0 {
		return errors.New("the database is still in use")
	}
	return nil
}

// ForgetApp deletes everything this computer remembers about an app: its
// install path, last launch, approvals and sync records.
func (s *Store) ForgetApp(appID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`DELETE FROM app_state WHERE id = ?`, appID); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM sync_records WHERE app_id = ?`, appID)
	return err
}
