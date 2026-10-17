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
