// Package observe watches which files an app writes, so the user can pick
// what to keep in the cloud without reading the app's code. It snapshots the
// app's folders before Start and after Stop, keeps per-file statistics across
// runs, and suggests "keep" or "temporary" with plain reasons.
package observe

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Where a file lives.
const (
	InData = "data" // the app's DATA_DIR
	InApp  = "app"  // the app's own folder (files git does not track)
)

// File is one file in a snapshot.
type File struct {
	Where   string    `json:"where"`
	Path    string    `json:"path"` // forward slashes, relative to its root
	Size    int64     `json:"size"`
	SHA     string    `json:"sha"` // content hash, or size+time for very large files
	ModTime time.Time `json:"modTime"`
	SQLite  bool      `json:"sqlite,omitempty"`
}

// Key identifies a file across snapshots.
func (f File) Key() string { return f.Where + ":" + f.Path }

// Snapshot maps Key -> File.
type Snapshot map[string]File

// heavy folders are skipped: dependencies, build caches and git internals.
var heavy = map[string]bool{".git": true, "node_modules": true, "vendor": true, "__pycache__": true,
	".venv": true, "venv": true, "target": true, ".next": true, ".vite": true}

const hashLimit = 32 << 20

// Take snapshots the data folder (all files) and the app folder (files git
// does not track; all files if the folder is not a git repository).
func Take(appDir, dataDir string) Snapshot {
	s := Snapshot{}
	walk(s, InData, dataDir, nil)
	var tracked map[string]bool
	if st, err := os.Stat(filepath.Join(appDir, ".git")); err == nil && st.IsDir() {
		tracked = gitTracked(appDir)
	}
	walk(s, InApp, appDir, tracked)
	return s
}

func gitTracked(dir string) map[string]bool {
	c := exec.Command("git", "-C", dir, "ls-files", "-z")
	var out bytes.Buffer
	c.Stdout = &out
	if c.Run() != nil {
		return nil
	}
	m := map[string]bool{}
	for _, p := range strings.Split(out.String(), "\x00") {
		if p != "" {
			m[p] = true
		}
	}
	return m
}

func walk(s Snapshot, where, root string, skip map[string]bool) {
	if root == "" {
		return
	}
	n := 0
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != root && heavy[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if skip[rel] || rel == ".mydailyapps-installed.json" {
			return nil
		}
		if n++; n > 50000 {
			return filepath.SkipAll
		}
		st, err := d.Info()
		if err != nil {
			return nil
		}
		f := File{Where: where, Path: rel, Size: st.Size(), ModTime: st.ModTime()}
		f.SHA, f.SQLite = hashFile(p, st)
		s[f.Key()] = f
		return nil
	})
}

var sqliteMagic = []byte("SQLite format 3\x00")

func hashFile(p string, st fs.FileInfo) (string, bool) {
	fh, err := os.Open(p)
	if err != nil {
		return "", false
	}
	defer fh.Close()
	head := make([]byte, 16)
	n, _ := io.ReadFull(fh, head)
	isSQLite := n == 16 && bytes.Equal(head, sqliteMagic)
	if st.Size() > hashLimit {
		return fmt.Sprintf("size:%d:%d", st.Size(), st.ModTime().UnixNano()), isSQLite
	}
	h := sha256.New()
	h.Write(head[:n])
	io.Copy(h, fh)
	return hex.EncodeToString(h.Sum(nil)), isSQLite
}

// Diff lists what changed between two snapshots.
type Diff struct {
	Added, Changed, Removed []File
}

// Compare returns what happened between before and after.
func Compare(before, after Snapshot) Diff {
	var d Diff
	for k, a := range after {
		b, ok := before[k]
		switch {
		case !ok:
			d.Added = append(d.Added, a)
		case b.SHA != a.SHA:
			d.Changed = append(d.Changed, a)
		}
	}
	for k, b := range before {
		if _, ok := after[k]; !ok {
			d.Removed = append(d.Removed, b)
		}
	}
	return d
}

// Stat is what is known about one file across runs.
type Stat struct {
	Where       string    `json:"where"`
	Path        string    `json:"path"`
	Size        int64     `json:"size"`
	SHA         string    `json:"sha"`
	SQLite      bool      `json:"sqlite,omitempty"`
	FirstRun    int       `json:"firstRun"`              // run number it appeared in (0 = before observing)
	ChangedRuns int       `json:"changedRuns"`           // runs in which it was added or changed
	Recreated   int       `json:"recreated,omitempty"`   // came back with identical content after being removed
	Removed     bool      `json:"removed,omitempty"`     // gone after the last run
	Recorded    bool      `json:"recorded,omitempty"`    // changed while the user recorded a save
	Verdict     string    `json:"verdict,omitempty"`     // "keep" or "temporary" from the set-aside test
	LastChanged time.Time `json:"lastChanged,omitempty"` // when a run last changed it
	LastRemoved string    `json:"lastRemovedSha,omitempty"`
	AsideSHA    string    `json:"asideSha,omitempty"` // set aside by the user (content hash)
	AsideRun    int       `json:"asideRun,omitempty"` // run counter when it was set aside
}

// Book is the per-app observation record, saved as JSON.
type Book struct {
	Runs  int              `json:"runs"`
	Files map[string]*Stat `json:"files"`
}

// LoadBook reads a book (empty if missing).
func LoadBook(p string) *Book {
	b := &Book{Files: map[string]*Stat{}}
	data, err := os.ReadFile(p)
	if err == nil {
		_ = json.Unmarshal(data, b)
	}
	if b.Files == nil {
		b.Files = map[string]*Stat{}
	}
	return b
}

// Save writes the book.
func (b *Book) Save(p string) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(b, "", " ")
	return os.WriteFile(p, data, 0o600)
}

func (b *Book) stat(f File) *Stat {
	s := b.Files[f.Key()]
	if s == nil {
		s = &Stat{Where: f.Where, Path: f.Path, FirstRun: b.Runs}
		b.Files[f.Key()] = s
	}
	s.Size, s.SHA, s.SQLite = f.Size, f.SHA, f.SQLite
	return s
}

// Seed adds files present now without counting a run (for the first view,
// before the app ran under observation). Data-folder files only: in the app
// folder, an untracked file is not necessarily written by the app.
func (b *Book) Seed(now Snapshot) {
	for _, f := range now {
		if _, ok := b.Files[f.Key()]; !ok && f.Where == InData {
			b.stat(f)
		}
	}
}

// AddRun records one run of the app.
func (b *Book) AddRun(before, after Snapshot, at time.Time) Diff {
	b.Runs++
	d := Compare(before, after)
	for _, f := range d.Added {
		s := b.stat(f)
		if (s.Removed && s.LastRemoved == f.SHA) || (s.AsideSHA != "" && s.AsideSHA == f.SHA) {
			s.Recreated++
		}
		s.Removed = false
		s.ChangedRuns++
		s.LastChanged = at
	}
	for _, f := range d.Changed {
		s := b.stat(f)
		s.ChangedRuns++
		s.LastChanged = at
	}
	for _, f := range d.Removed {
		if s := b.Files[f.Key()]; s != nil {
			s.Removed, s.LastRemoved = true, f.SHA
		}
	}
	// Data-folder files that existed all along are known too.
	b.Seed(after)
	return d
}

// MarkRecorded flags files that changed while the user recorded a save.
func (b *Book) MarkRecorded(d Diff) []string {
	var out []string
	for _, f := range append(append([]File{}, d.Added...), d.Changed...) {
		s := b.stat(f)
		s.Recorded = true
		out = append(out, f.Key())
	}
	sort.Strings(out)
	return out
}

// Suggestion is the keep/temporary verdict for one file.
type Suggestion struct {
	Keep    bool     `json:"keep"`
	Reasons []string `json:"reasons"`
}

var tempSegs = map[string]bool{"cache": true, ".cache": true, "caches": true, "tmp": true, "temp": true, "log": true,
	"logs": true, "dist": true, "build": true, "out": true, "coverage": true, "thumbs": true, "thumbnails": true, ".parcel-cache": true}
var tempExts = map[string]bool{".log": true, ".tmp": true, ".temp": true, ".lock": true, ".pid": true, ".swp": true, ".bak": true, ".map": true}
var keepSegs = map[string]bool{"data": true, "saves": true, "save": true, "db": true, "database": true, "storage": true,
	"userdata": true, "user-data": true, "profiles": true, "uploads": true, "documents": true}
var keepExts = map[string]bool{".db": true, ".sqlite": true, ".sqlite3": true, ".sav": true, ".save": true}
var docExts = map[string]bool{".json": true, ".yaml": true, ".yml": true, ".toml": true, ".txt": true, ".md": true, ".csv": true, ".xml": true, ".ini": true}

// Suggest scores a file from its name, place and behaviour across runs.
func Suggest(s *Stat, runs int) Suggestion {
	score := 0
	var why []string
	add := func(n int, reason string) { score += n; why = append(why, reason) }

	if s.Verdict == "keep" {
		return Suggestion{true, []string{"You checked: your data was gone without it"}}
	}
	if s.Verdict == "temporary" {
		return Suggestion{false, []string{"You checked: the app worked fine without it"}}
	}
	if s.Recorded {
		add(6, "Changed while you recorded a save")
	}
	segs := strings.Split(s.Path, "/")
	ext := strings.ToLower(path.Ext(s.Path))
	base := strings.ToLower(path.Base(s.Path))
	tempName, keepName := "", ""
	for _, seg := range segs[:len(segs)-1] {
		l := strings.ToLower(seg)
		if tempSegs[l] && tempName == "" {
			tempName = seg
		}
		if keepSegs[l] && keepName == "" {
			keepName = seg
		}
	}
	switch {
	case strings.HasSuffix(base, "-wal") || strings.HasSuffix(base, "-shm") || strings.HasSuffix(base, "-journal"):
		add(-6, "Database side file (merged into the database itself)")
	case tempExts[ext]:
		add(-3, fmt.Sprintf("%s files are usually temporary", ext))
	case tempName != "":
		add(-3, fmt.Sprintf("In a %q folder", tempName))
	}
	if s.SQLite {
		add(3, "SQLite database")
	} else if keepExts[ext] {
		add(2, fmt.Sprintf("%s file", ext))
	}
	if keepName != "" {
		add(2, fmt.Sprintf("In a %q folder", keepName))
	}
	if s.Where == InData {
		add(1, "In the app's data folder")
		if docExts[ext] && tempName == "" {
			add(1, "A document-like file")
		}
	}
	if s.Recreated > 0 {
		add(-4, "Came back with the same content after it was removed (regenerated)")
	}
	if runs >= 3 && s.ChangedRuns >= runs && !s.Recorded {
		// Logs and caches change every run; so do autosaves, so a save-like
		// name or type weakens this signal.
		if keepName != "" || keepExts[ext] || s.SQLite {
			add(-1, "Changes on every run (could be an autosave)")
		} else {
			add(-2, "Changes on every run, whatever you do")
		}
	}
	if s.Size > 100<<20 {
		add(-1, "Very large")
	}
	if len(why) == 0 {
		why = []string{"Nothing points either way"}
	}
	return Suggestion{Keep: score > 0, Reasons: why}
}

// Patterns turns chosen files into cloud patterns and the root they are
// relative to. Folders whose observed files are all chosen become "folder/**".
func Patterns(chosen []*Stat, all map[string]*Stat) (root string, include, sqlite []string, err error) {
	if len(chosen) == 0 {
		return "", nil, nil, nil
	}
	root = chosen[0].Where
	for _, s := range chosen {
		if s.Where != root {
			return "", nil, nil, errors.New("choose files from one place: either the app's data folder or its own folder")
		}
	}
	chosenSet := map[string]bool{}
	for _, s := range chosen {
		chosenSet[s.Path] = true
		if s.SQLite {
			sqlite = append(sqlite, s.Path)
		}
	}
	// A top folder is "whole" if every observed file under it is chosen.
	whole := map[string]bool{}
	partial := map[string]bool{}
	for _, s := range all {
		if s.Where != root || !strings.Contains(s.Path, "/") {
			continue
		}
		top := strings.SplitN(s.Path, "/", 2)[0]
		if chosenSet[s.Path] {
			if !partial[top] {
				whole[top] = true
			}
		} else {
			partial[top] = true
			delete(whole, top)
		}
	}
	seen := map[string]bool{}
	for _, s := range chosen {
		p := s.Path
		if top, _, ok := strings.Cut(p, "/"); ok && whole[top] {
			p = top + "/**"
		}
		if !seen[p] {
			seen[p] = true
			include = append(include, p)
		}
	}
	sort.Strings(include)
	sort.Strings(sqlite)
	if root == InData {
		root = ""
	}
	return root, include, sqlite, nil
}
