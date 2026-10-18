package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/robiwizzard/mydailyapps/internal/detect"
	"github.com/robiwizzard/mydailyapps/internal/install"
	"github.com/robiwizzard/mydailyapps/internal/library"
)

// The library entry is the source of truth for an app's configuration. These
// functions add, edit and remove entries and save the library locally and to
// Drive.

// saveLibrary stores the library locally and in Drive (when connected).
func (h *Host) saveLibrary(ctx context.Context, l *library.Library, src string) error {
	if err := l.Validate(); err != nil {
		return err
	}
	if err := h.Lib.Set(l, src); err != nil {
		return err
	}
	h.Rescan()
	if h.Drive.Connected() {
		data, _ := l.Marshal()
		ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		if err := h.Drive.UploadFile(ctx, library.FileName, "application/json", data); err != nil {
			return fmt.Errorf("saved on this computer, but saving to Drive failed: %w", err)
		}
	}
	return nil
}

// ImportLibrary replaces the library (and saves it to Drive if connected).
func (h *Host) ImportLibrary(ctx context.Context, b []byte) error {
	l, err := library.Parse(b)
	if err != nil {
		return err
	}
	return h.saveLibrary(ctx, l, "imported file")
}

// Entry returns an app's effective configuration for the Edit form: the
// library entry with any fields from the repository's manifest filled in.
func (h *Host) Entry(id string) (library.App, error) {
	return h.app(id)
}

// SaveEntry adds a new entry (originalID == "") or replaces an existing one.
func (h *Host) SaveEntry(ctx context.Context, originalID string, e library.App) error {
	e.ID = strings.TrimSpace(e.ID)
	e.Name = strings.TrimSpace(e.Name)
	e.Group = strings.TrimSpace(e.Group)
	if e.Name == "" {
		return errors.New("name is required")
	}
	if e.Cloud != nil && len(e.Cloud.Include) == 0 {
		e.Cloud = nil // an empty cloud section means "sync nothing"
	}
	cur, _ := h.Lib.Get()
	l := &library.Library{Version: cur.Version}
	found := false
	var oldCloud *library.Cloud
	for _, a := range cur.Apps {
		switch {
		case originalID != "" && a.ID == originalID:
			if e.ID != originalID {
				return errors.New("the id cannot be changed (it names the app's folders); remove the app and add it again instead")
			}
			l.Apps = append(l.Apps, e)
			found = true
			oldCloud = a.Cloud
		case a.ID == e.ID && originalID == "":
			return fmt.Errorf("an app with the id %q is already in the library", e.ID)
		default:
			l.Apps = append(l.Apps, a)
		}
	}
	if originalID != "" && !found {
		return ErrNotFound
	}
	if originalID == "" {
		l.Apps = append(l.Apps, e)
	}
	if err := h.saveLibrary(ctx, l, "edited in MyDailyApps"); err != nil {
		return err
	}
	h.saveAppMeta(ctx, e)
	if !sameCloud(oldCloud, e.Cloud) {
		if oldCloud != nil && e.Cloud != nil && oldCloud.AppRoot() != e.Cloud.AppRoot() {
			// The paths are now relative to another folder: compare both
			// sides from scratch instead of treating files as deleted.
			_ = h.Store.ClearSyncRecords(e.ID)
		}
		h.syncAfterChange(e.ID)
	}
	return nil
}

func sameCloud(a, b *library.Cloud) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// appMeta is the settings copy kept in the app's Drive folder.
type appMeta struct {
	Entry   library.App `json:"entry"`
	SavedAt time.Time   `json:"savedAt"`
}

// saveAppMeta keeps a copy of the entry in the app's Drive folder, so a later
// "Add app" of the same app finds its settings and data again. Best effort.
func (h *Host) saveAppMeta(ctx context.Context, e library.App) {
	if !h.Drive.Connected() {
		return
	}
	b, _ := json.MarshalIndent(appMeta{Entry: e, SavedAt: time.Now()}, "", "  ")
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := h.Drive.SaveAppMeta(ctx, e.ID, b); err != nil {
		log.Printf("saving settings copy for %s: %v", e.ID, err)
	}
}

// RemoveEntry takes an app out of the library. Its folder and data stay.
func (h *Host) RemoveEntry(ctx context.Context, id string) error {
	if st, _ := h.Procs.Status(id); st != "" {
		return errors.New("stop the app first")
	}
	cur, _ := h.Lib.Get()
	l := &library.Library{Version: cur.Version}
	found := false
	for _, a := range cur.Apps {
		if a.ID == id {
			found = true
			continue
		}
		l.Apps = append(l.Apps, a)
	}
	if !found {
		return ErrNotFound
	}
	// Make sure Drive keeps the latest settings, so adding the app again
	// later finds them together with its data.
	if e, err := h.app(id); err == nil {
		h.saveAppMeta(ctx, e)
	}
	return h.saveLibrary(ctx, l, "edited in MyDailyApps")
}

// Detect drafts an entry. With an installed app's id it reads the app's
// folder; otherwise it reads the GitHub repository without cloning it.
func (h *Host) Detect(ctx context.Context, repo, id, token string) (detect.Draft, error) {
	groups := h.Groups()
	if id != "" {
		a, err := h.app(id)
		if err != nil {
			return detect.Draft{}, err
		}
		s, _ := h.Store.App(id)
		dir := install.Folder(h.Cfg.AppsDir, s.InstallPath, id)
		if install.Scan(a, dir).Installed {
			src, err := detect.Local(dir)
			if err != nil {
				return detect.Draft{}, err
			}
			d := detect.Detect(src, a.Repo, groups)
			d.App.ID = id // the folder name is not the repo name
			if a.Name != "" {
				d.App.Name = a.Name
			}
			if a.Group != "" {
				d.App.Group = a.Group
			}
			if a.Description != "" {
				d.App.Description = a.Description
			}
			return d, nil
		}
		if repo == "" {
			repo = a.Repo
		}
	}
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return detect.Draft{}, errors.New("enter the repository link")
	}
	tok := h.tokenFor(repo, token)
	src, err := detect.GitHub(ctx, repo, tok)
	if err != nil {
		if errors.Is(err, detect.ErrNotFound) && tok == "" {
			return detect.Draft{}, &TokenError{Message: "The repository was not found. If it is private, enter a GitHub token (used for this session only)."}
		}
		return detect.Draft{}, err
	}
	d := detect.Detect(src, repo, groups)
	if id != "" {
		d.App.ID = id
	} else {
		h.restoreFromDrive(ctx, &d)
	}
	return d, nil
}

// restoreFromDrive looks for this app's earlier folder in Drive (same id, or
// a settings copy with the same repository) and, if found, brings back the
// settings that were used before, so the old data is synced again.
func (h *Host) restoreFromDrive(ctx context.Context, d *detect.Draft) {
	if !h.Drive.Connected() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	try := func(id string) bool {
		info, err := h.Drive.AppFolder(ctx, id)
		if err != nil || !info.Exists {
			return false
		}
		var m appMeta
		hasMeta := json.Unmarshal(info.Meta, &m) == nil && m.Entry.ID == id
		if hasMeta && m.Entry.Repo != "" && d.App.Repo != "" && !sameRepo(m.Entry.Repo, d.App.Repo) {
			return false // the same id, but a different app
		}
		d.App.ID = id
		if hasMeta {
			prev := m.Entry
			if prev.Name != "" {
				d.App.Name = prev.Name
			}
			if prev.Group != "" {
				d.App.Group = prev.Group
			}
			if prev.Description != "" {
				d.App.Description = prev.Description
			}
			if prev.Port != 0 {
				d.App.Port = prev.Port
			}
			if prev.Path != "" {
				d.App.Path = prev.Path
			}
			if prev.Cloud != nil {
				d.App.Cloud = prev.Cloud
			}
			if len(prev.Install) > 0 {
				d.App.Install = prev.Install
			}
			d.Findings = append([]detect.Finding{{Field: "all", Confidence: "detected", Note: fmt.Sprintf(
				"This app was in your library before. Its earlier settings (saved %s) and its %d synced file(s) in Google Drive (MyDailyApps/apps/%s) were found and will be used again.",
				m.SavedAt.Local().Format("2006-01-02"), info.Files, id)}}, d.Findings...)
		} else if info.Files > 0 {
			// Saved by an older version: rebuild the sync patterns from the files themselves.
			c := &library.Cloud{Include: patternsFor(info.Paths)}
			if d.App.Cloud != nil {
				c.Root = d.App.Cloud.Root
			}
			d.App.Cloud = c
			d.Findings = append([]detect.Finding{{Field: "all", Confidence: "guessed", Note: fmt.Sprintf(
				"Google Drive already has %d synced file(s) for this app (MyDailyApps/apps/%s). Cloud sync was set to the same files (%s) so they come back on the first start; check where they belong under Cloud sync.",
				info.Files, id, strings.Join(c.Include, ", "))}}, d.Findings...)
		} else {
			d.Findings = append([]detect.Finding{{Field: "all", Confidence: "guessed", Note: fmt.Sprintf(
				"Google Drive has an empty folder for this app (MyDailyApps/apps/%s); it will be used again.", id)}}, d.Findings...)
		}
		return true
	}
	if try(d.App.ID) {
		return
	}
	// The app may have had another id before: look for a settings copy with the same repository.
	ids, err := h.Drive.AppFolderIDs(ctx)
	if err != nil {
		return
	}
	cur, _ := h.Lib.Get()
	for _, id := range ids {
		if _, inLib := cur.Find(id); inLib || id == d.App.ID {
			continue
		}
		info, err := h.Drive.AppFolder(ctx, id)
		if err != nil || len(info.Meta) == 0 {
			continue
		}
		var m appMeta
		if json.Unmarshal(info.Meta, &m) == nil && sameRepo(m.Entry.Repo, d.App.Repo) && try(id) {
			return
		}
	}
}

// patternsFor groups synced file paths by their top folder ("saves/**").
func patternsFor(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		pat := p
		if top, _, ok := strings.Cut(p, "/"); ok {
			pat = top + "/**"
		}
		if !seen[pat] {
			seen[pat] = true
			out = append(out, pat)
		}
	}
	sort.Strings(out)
	return out
}

// SaveAllAppMeta refreshes the settings copy in Drive for every app that
// syncs files (so apps set up before this existed get one). Background.
func (h *Host) SaveAllAppMeta(ctx context.Context) {
	if !h.Drive.Connected() {
		return
	}
	l, _ := h.Lib.Get()
	for _, e := range l.Apps {
		if a, err := h.app(e.ID); err == nil && a.Cloud != nil {
			h.saveAppMeta(ctx, a)
		}
	}
}

func sameRepo(a, b string) bool {
	norm := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		s = strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
		s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
		return strings.TrimPrefix(s, "www.")
	}
	return norm(a) != "" && norm(a) == norm(b)
}
