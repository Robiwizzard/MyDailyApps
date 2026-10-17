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

// App is one library entry. The same fields appear in an app's own
// mydailyapps.json manifest; a library entry can be as small as id + repo,
// and any field it does set overrides the manifest.
type App struct {
	ID          string          `json:"id"`
	Name        string          `json:"name,omitempty"`
	Group       string          `json:"group,omitempty"`
	Description string          `json:"description,omitempty"`
	Repo        string          `json:"repo,omitempty"`
	Port        int             `json:"port,omitempty"` // fixed port, if the app cannot read PORT
	Path        string          `json:"path,omitempty"` // URL path to open, default "/"
	Install     []InstallOption `json:"install,omitempty"`
	Cloud       *Cloud          `json:"cloud,omitempty"`
}

// Cloud lists the files that are synced to Drive. Patterns are relative to
// the app's DATA_DIR, or to the app's own folder when Root is "app" (for apps
// that keep their data next to their code instead of reading DATA_DIR).
type Cloud struct {
	Root      string   `json:"root,omitempty"` // "" or "data" = DATA_DIR, "app" = the app folder
	Include   []string `json:"include"`
	Exclude   []string `json:"exclude,omitempty"`
	SQLite    []string `json:"sqlite,omitempty"`
	MaxSizeMB int      `json:"maxSizeMB,omitempty"`
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
		if err := a.validateFields(where); err != nil {
			return err
		}
	}
	return nil
}

// validateFields checks the fields both library entries and manifests share.
func (a *App) validateFields(where string) error {
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
	}
	if a.Cloud != nil {
		if err := a.Cloud.Validate(); err != nil {
			return fmt.Errorf("%s: cloud: %w", where, err)
		}
	}
	return nil
}

// Validate checks that every pattern stays inside DATA_DIR.
func (c *Cloud) Validate() error {
	if len(c.Include) == 0 {
		return errors.New("include must list at least one pattern")
	}
	all := append(append(append([]string{}, c.Include...), c.Exclude...), c.SQLite...)
	for _, p := range all {
		if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || filepath.IsAbs(p) ||
			strings.Contains(p, ":") {
			return fmt.Errorf("%q must be a relative path with forward slashes", p)
		}
		for _, seg := range strings.Split(p, "/") {
			if seg == ".." || seg == "." {
				return fmt.Errorf("%q must not contain . or .. segments", p)
			}
		}
	}
	if c.Root != "" && c.Root != "data" && c.Root != "app" {
		return errors.New(`root must be "data" or "app"`)
	}
	if c.MaxSizeMB < 0 {
		return errors.New("maxSizeMB must not be negative")
	}
	return nil
}

// Merge returns the manifest with every field the library entry sets laid
// over it. Either may be nil-ish (zero App).
func Merge(entry App, manifest *App) App {
	if manifest == nil {
		out := entry
		if out.Name == "" {
			out.Name = out.ID
		}
		return out
	}
	out := *manifest
	out.ID = entry.ID
	if entry.Name != "" {
		out.Name = entry.Name
	}
	if entry.Group != "" {
		out.Group = entry.Group
	}
	if entry.Description != "" {
		out.Description = entry.Description
	}
	if entry.Repo != "" {
		out.Repo = entry.Repo
	}
	if entry.Port != 0 {
		out.Port = entry.Port
	}
	if entry.Path != "" {
		out.Path = entry.Path
	}
	if len(entry.Install) > 0 {
		out.Install = entry.Install
	}
	if entry.Cloud != nil {
		out.Cloud = entry.Cloud
	}
	if out.Name == "" {
		out.Name = out.ID
	}
	return out
}

// AppRoot reports whether the patterns are relative to the app folder.
func (c *Cloud) AppRoot() bool { return c != nil && c.Root == "app" }

// ManifestName is the file an app ships in its repository root.
const ManifestName = "mydailyapps.json"

// Manifest is an app's own description of itself.
type Manifest struct {
	ManifestVersion int `json:"manifestVersion"`
	App
}

// ParseManifest decodes and validates a mydailyapps.json file.
func ParseManifest(b []byte) (*App, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", ManifestName, err)
	}
	if m.ManifestVersion > 1 {
		return nil, fmt.Errorf("%s uses manifestVersion %d; this MyDailyApps understands version 1", ManifestName, m.ManifestVersion)
	}
	if m.ID != "" && !idRe.MatchString(m.ID) {
		return nil, fmt.Errorf("%s: invalid id %q", ManifestName, m.ID)
	}
	if err := m.App.validateFields(ManifestName); err != nil {
		return nil, err
	}
	return &m.App, nil
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
