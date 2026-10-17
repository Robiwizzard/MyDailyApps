// Package library defines the library file (which apps exist, their groups,
// and how to install and launch them) and its local cache.
package library

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// FileName is the name of the library file in Drive and in the local cache.
const FileName = "library.json"

// InstallOption is one way to install and run an app. Options are tried in order.
type InstallOption struct {
	Type     string   `json:"type"`               // "release" or "git"
	Requires []string `json:"requires,omitempty"` // tools that must be on the PATH, e.g. ["node"]
	Setup    string   `json:"setup,omitempty"`    // run once after clone/unpack, in the app folder
	Launch   string   `json:"launch"`             // start command, run in the app folder
	// Release only: substring the asset name must contain (optional). The host
	// otherwise picks the asset that matches this OS and architecture.
	Asset string `json:"asset,omitempty"`
	// Release only: GitHub release tag (default: latest).
	Tag string `json:"tag,omitempty"`
}

// App is one library entry.
type App struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Group       string          `json:"group"`
	Description string          `json:"description,omitempty"`
	Repo        string          `json:"repo,omitempty"`
	Port        int             `json:"port,omitempty"` // fixed port, if the app cannot read PORT
	Path        string          `json:"path,omitempty"` // URL path to open, default "/"
	Install     []InstallOption `json:"install"`
}

// Library is the whole file.
type Library struct {
	Version int   `json:"version"`
	Apps    []App `json:"apps"`
}

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Validate checks the file and returns the first problem found.
func (l *Library) Validate() error {
	seen := map[string]bool{}
	for i, a := range l.Apps {
		where := fmt.Sprintf("app %d (%s)", i+1, a.ID)
		if !idRe.MatchString(a.ID) {
			return fmt.Errorf("%s: id must be lower case letters, digits, '.', '_' or '-'", where)
		}
		if seen[a.ID] {
			return fmt.Errorf("%s: duplicate id", where)
		}
		seen[a.ID] = true
		if strings.TrimSpace(a.Name) == "" {
			return fmt.Errorf("%s: name is required", where)
		}
		if a.Port < 0 || a.Port > 65535 {
			return fmt.Errorf("%s: port out of range", where)
		}
		for j, o := range a.Install {
			if o.Type != "release" && o.Type != "git" {
				return fmt.Errorf("%s install option %d: type must be release or git", where, j+1)
			}
			if strings.TrimSpace(o.Launch) == "" {
				return fmt.Errorf("%s install option %d: launch is required", where, j+1)
			}
			if a.Repo == "" {
				return fmt.Errorf("%s: repo is required for %s installs", where, o.Type)
			}
		}
	}
	return nil
}

// Find returns the app with the id.
func (l *Library) Find(id string) (App, bool) {
	for _, a := range l.Apps {
		if a.ID == id {
			return a, true
		}
	}
	return App{}, false
}

// Parse decodes and validates library JSON.
func Parse(b []byte) (*Library, error) {
	var l Library
	dec := json.NewDecoder(strings.NewReader(string(b)))
	if err := dec.Decode(&l); err != nil {
		return nil, fmt.Errorf("library file is not valid JSON: %w", err)
	}
	if l.Apps == nil {
		l.Apps = []App{}
	}
	if err := l.Validate(); err != nil {
		return nil, err
	}
	return &l, nil
}

// Marshal encodes the library for saving.
func (l *Library) Marshal() ([]byte, error) {
	if l.Version == 0 {
		l.Version = 1
	}
	return json.MarshalIndent(l, "", "  ")
}

// Cache holds the current library and its local copy on disk.
type Cache struct {
	path string
	mu   sync.RWMutex
	lib  *Library
	src  string // where the current copy came from: drive, cache, seed, empty
}

func NewCache(dataDir string) *Cache {
	return &Cache{path: filepath.Join(dataDir, FileName), lib: &Library{Version: 1, Apps: []App{}}, src: "empty"}
}

// Get returns the current library (do not modify it).
func (c *Cache) Get() (*Library, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lib, c.src
}

// LoadLocal loads the cached copy; if there is none it falls back to a seed
// file (for example library.json next to the executable).
func (c *Cache) LoadLocal(seedPaths ...string) error {
	b, err := os.ReadFile(c.path)
	src := "cache"
	if errors.Is(err, os.ErrNotExist) {
		for _, p := range seedPaths {
			if b, err = os.ReadFile(p); err == nil {
				src = "file " + p
				break
			}
		}
		if err != nil {
			return nil // nothing yet: empty library
		}
	}
	if err != nil {
		return err
	}
	l, err := Parse(b)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.lib, c.src = l, src
	c.mu.Unlock()
	return nil
}

// HasLocal reports whether a cached copy exists on disk.
func (c *Cache) HasLocal() bool {
	_, err := os.Stat(c.path)
	return err == nil
}

// Set replaces the library and writes the local cache.
func (c *Cache) Set(l *Library, src string) error {
	b, err := l.Marshal()
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, c.path); err != nil {
		return err
	}
	c.mu.Lock()
	c.lib, c.src = l, src
	c.mu.Unlock()
	return nil
}
