package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/robiwizzard/mydailyapps/internal/cloudsync"
	"github.com/robiwizzard/mydailyapps/internal/install"
	"github.com/robiwizzard/mydailyapps/internal/library"
	"github.com/robiwizzard/mydailyapps/internal/observe"
)

// The Files view: what an app writes, a keep/temporary suggestion for each
// file, "record a save", and the set-aside test. Records live in
// <data dir>/observe/<id>.json; set-aside files in <data dir>/aside/<id>/.

func (h *Host) bookPath(id string) string { return filepath.Join(h.Cfg.DataDir, "observe", id+".json") }

func (h *Host) asidePath(id, where, rel string) string {
	return filepath.Join(h.Cfg.DataDir, "aside", id, where, filepath.FromSlash(rel))
}

func (h *Host) dirs(id string) (appDir, dataDir string) {
	s, _ := h.Store.App(id)
	return install.Folder(h.Cfg.AppsDir, s.InstallPath, id), h.Cfg.AppDataDir(id)
}

func (h *Host) observeBefore(id, appDir, dataDir string) {
	snap := observe.Take(appDir, dataDir)
	h.mu.Lock()
	h.obsBefore[id] = snap
	h.mu.Unlock()
}

func (h *Host) observeAfter(id, appDir, dataDir string) {
	h.mu.Lock()
	before, ok := h.obsBefore[id]
	delete(h.obsBefore, id)
	delete(h.recording, id)
	h.mu.Unlock()
	if !ok {
		return
	}
	h.obsMu.Lock()
	defer h.obsMu.Unlock()
	b := observe.LoadBook(h.bookPath(id))
	b.AddRun(before, observe.Take(appDir, dataDir), time.Now())
	_ = b.Save(h.bookPath(id))
}

// FileView is one row in the Files view.
type FileView struct {
	Key      string   `json:"key"`
	Where    string   `json:"where"` // data or app
	Path     string   `json:"path"`
	Size     int64    `json:"size"`
	SQLite   bool     `json:"sqlite,omitempty"`
	Keep     bool     `json:"keep"`
	Reasons  []string `json:"reasons"`
	Synced   bool     `json:"synced"`             // matched by the current cloud settings
	Aside    bool     `json:"aside,omitempty"`    // set aside by the user
	Question bool     `json:"question,omitempty"` // set aside and the app ran since: ask the user
	Gone     bool     `json:"gone,omitempty"`     // no longer on disk
}

// FilesInfo is the GET /api/apps/{id}/files payload.
type FilesInfo struct {
	Runs      int        `json:"runs"`
	Running   bool       `json:"running"`
	Recording bool       `json:"recording"`
	Root      string     `json:"root"` // current cloud root: data or app
	Files     []FileView `json:"files"`
}

// Files lists what is known about the files an app writes.
func (h *Host) Files(id string) (FilesInfo, error) {
	a, err := h.app(id)
	if err != nil {
		return FilesInfo{}, err
	}
	appDir, dataDir := h.dirs(id)
	h.obsMu.Lock()
	b := observe.LoadBook(h.bookPath(id))
	now := observe.Take(appDir, dataDir)
	b.Seed(now)
	_ = b.Save(h.bookPath(id))
	h.obsMu.Unlock()

	st, _ := h.Procs.Status(id)
	h.mu.Lock()
	_, rec := h.recording[id]
	h.mu.Unlock()
	info := FilesInfo{Runs: b.Runs, Running: st != "", Recording: rec, Root: observe.InData}
	if a.Cloud.AppRoot() {
		info.Root = observe.InApp
	}
	for k, s := range b.Files {
		_, onDisk := now[k]
		aside := s.AsideSHA != ""
		if !onDisk && !aside && s.Removed {
			continue // the app deleted it: temporary by definition, nothing to choose
		}
		sug := observe.Suggest(s, b.Runs)
		inRoot := (s.Where == observe.InApp) == a.Cloud.AppRoot()
		info.Files = append(info.Files, FileView{
			Key: k, Where: s.Where, Path: s.Path, Size: s.Size, SQLite: s.SQLite,
			Keep: sug.Keep, Reasons: sug.Reasons,
			Synced: a.Cloud != nil && inRoot && cloudsync.Matches(a.Cloud, s.Path),
			Aside:  aside, Question: aside && b.Runs > s.AsideRun, Gone: !onDisk && !aside,
		})
	}
	sort.Slice(info.Files, func(i, j int) bool {
		if info.Files[i].Where != info.Files[j].Where {
			return info.Files[i].Where < info.Files[j].Where
		}
		return info.Files[i].Path < info.Files[j].Path
	})
	return info, nil
}

// Record starts or stops "record a save" while the app runs. Stopping marks
// the files that changed in between and returns their keys.
func (h *Host) Record(id string, start bool) ([]string, error) {
	if _, err := h.app(id); err != nil {
		return nil, err
	}
	if st, _ := h.Procs.Status(id); st != "running" {
		return nil, errors.New("start the app first, then record a save while it runs")
	}
	appDir, dataDir := h.dirs(id)
	if start {
		snap := observe.Take(appDir, dataDir)
		h.mu.Lock()
		h.recording[id] = snap
		h.mu.Unlock()
		return nil, nil
	}
	h.mu.Lock()
	before, ok := h.recording[id]
	delete(h.recording, id)
	h.mu.Unlock()
	if !ok {
		return nil, errors.New("recording was not started")
	}
	h.obsMu.Lock()
	defer h.obsMu.Unlock()
	b := observe.LoadBook(h.bookPath(id))
	keys := b.MarkRecorded(observe.Compare(before, observe.Take(appDir, dataDir)))
	return keys, b.Save(h.bookPath(id))
}

// Aside handles the set-aside test for one file:
//   - "aside": move the file out of the app's reach (the app must be stopped)
//   - "restore": put it back
//   - "keep" / "temporary": the user's answer after running the app without it
func (h *Host) Aside(id, key, action string) error {
	if _, err := h.app(id); err != nil {
		return err
	}
	if st, _ := h.Procs.Status(id); st != "" {
		return errors.New("stop the app first")
	}
	where, rel, ok := strings.Cut(key, ":")
	if !ok || (where != observe.InData && where != observe.InApp) || strings.Contains(rel, "..") {
		return errors.New("invalid file")
	}
	appDir, dataDir := h.dirs(id)
	root := dataDir
	if where == observe.InApp {
		root = appDir
	}
	live := filepath.Join(root, filepath.FromSlash(rel))
	aside := h.asidePath(id, where, rel)

	h.obsMu.Lock()
	defer h.obsMu.Unlock()
	b := observe.LoadBook(h.bookPath(id))
	s := b.Files[key]
	if s == nil {
		return errors.New("unknown file")
	}
	restore := func() error {
		if _, err := os.Stat(aside); err != nil {
			return nil
		}
		if _, err := os.Stat(live); err == nil {
			// The app made a new one meanwhile; keep it next to the original.
			_ = os.Rename(live, live+".recreated")
		}
		if err := os.MkdirAll(filepath.Dir(live), 0o755); err != nil {
			return err
		}
		return os.Rename(aside, live)
	}
	switch action {
	case "aside":
		if err := os.MkdirAll(filepath.Dir(aside), 0o700); err != nil {
			return err
		}
		if err := os.Rename(live, aside); err != nil {
			return err
		}
		s.AsideSHA, s.AsideRun, s.Verdict = s.SHA, b.Runs, ""
		if s.AsideSHA == "" {
			s.AsideSHA = "set-aside"
		}
	case "restore":
		if err := restore(); err != nil {
			return err
		}
		s.AsideSHA = ""
	case "keep":
		if err := restore(); err != nil {
			return err
		}
		s.AsideSHA, s.Verdict = "", "keep"
	case "temporary":
		_ = os.Remove(aside)
		s.AsideSHA, s.Verdict = "", "temporary"
	default:
		return errors.New("unknown action")
	}
	return b.Save(h.bookPath(id))
}

// ApplyFiles turns the chosen files into the app's cloud settings and saves
// them in its library entry.
func (h *Host) ApplyFiles(ctx context.Context, id string, keys []string) (library.App, error) {
	a, err := h.app(id)
	if err != nil {
		return a, err
	}
	h.obsMu.Lock()
	b := observe.LoadBook(h.bookPath(id))
	h.obsMu.Unlock()
	var chosen []*observe.Stat
	for _, k := range keys {
		if s := b.Files[k]; s != nil {
			chosen = append(chosen, s)
		}
	}
	root, include, sqlite, err := observe.Patterns(chosen, b.Files)
	if err != nil {
		return a, err
	}
	if len(include) == 0 {
		a.Cloud = nil
	} else {
		c := &library.Cloud{Root: root, Include: include, SQLite: sqlite}
		if a.Cloud != nil {
			c.MaxSizeMB = a.Cloud.MaxSizeMB
		}
		a.Cloud = c
	}
	if err := h.SaveEntry(ctx, id, a); err != nil {
		return a, err
	}
	return a, nil
}
