package host

import (
	"context"
	"errors"
	"fmt"
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
	for _, a := range cur.Apps {
		switch {
		case originalID != "" && a.ID == originalID:
			if e.ID != originalID {
				return errors.New("the id cannot be changed (it names the app's folders); remove the app and add it again instead")
			}
			l.Apps = append(l.Apps, e)
			found = true
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
	return h.saveLibrary(ctx, l, "edited in MyDailyApps")
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
	}
	return d, nil
}
