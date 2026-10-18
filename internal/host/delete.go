package host

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/robiwizzard/mydailyapps/internal/config"
	"github.com/robiwizzard/mydailyapps/internal/install"
	"github.com/robiwizzard/mydailyapps/internal/library"
)

// DeleteApp removes an app completely: the library entry, its folder, its
// data folder and everything MyDailyApps remembers about it on this
// computer, and its Drive folders (moved to the Drive trash). It returns
// what was done, step by step.
func (h *Host) DeleteApp(ctx context.Context, id string) ([]string, error) {
	cur, _ := h.Lib.Get()
	if _, ok := cur.Find(id); !ok {
		return nil, ErrNotFound
	}
	if st, _ := h.Procs.Status(id); st != "" {
		return nil, errors.New("stop the app first")
	}
	h.mu.Lock()
	busy := h.installing[id] || h.updating[id] || h.syncing[id]
	h.mu.Unlock()
	if busy {
		return nil, errors.New("the app is busy (installing, updating or syncing); try again in a moment")
	}
	s, _ := h.Store.App(id)
	appDir := install.Folder(h.Cfg.AppsDir, s.InstallPath, id)
	if err := h.safeToDelete(appDir, s.InstallPath != ""); err != nil {
		return nil, err
	}
	var report []string
	step := func(what string, err error) {
		if err != nil {
			report = append(report, fmt.Sprintf("%s: failed (%v)", what, err))
		} else {
			report = append(report, what+": done")
		}
	}

	// Drive first: if it fails, the local copy still exists to try again.
	if h.Drive.Connected() {
		dctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		n, err := h.Drive.TrashApp(dctx, id)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("could not delete the app's Google Drive folders, so nothing was deleted: %w", err)
		}
		switch n {
		case 0:
			report = append(report, "Google Drive: there were no folders for this app")
		case 1:
			report = append(report, "Google Drive: MyDailyApps/apps/"+id+" moved to the Drive trash (Google keeps it there for 30 days)")
		default:
			report = append(report, "Google Drive: MyDailyApps/apps/"+id+" and MyDailyApps/backups/"+id+" moved to the Drive trash (Google keeps them there for 30 days)")
		}
	} else if h.Drive.Configured() {
		report = append(report, "Google Drive is not connected, so its folders for this app were not deleted (sign in and use Delete again on a re-added app, or delete MyDailyApps/apps/"+id+" in Drive)")
	}

	if _, err := os.Stat(appDir); err == nil {
		step("App folder "+appDir, removeAllForce(appDir))
	}
	if d := h.Cfg.AppDataDir(id); exists(d) {
		step("Data folder "+d, removeAllForce(d))
	}
	for _, p := range []string{h.bookPath(id), h.manifestCachePath(id)} {
		if exists(p) {
			_ = os.Remove(p)
		}
	}
	if d := filepath.Join(h.Cfg.DataDir, "aside", id); exists(d) {
		_ = removeAllForce(d)
	}
	step("What this computer remembered about the app", h.Store.ForgetApp(id))

	l := &library.Library{Version: cur.Version}
	for _, a := range cur.Apps {
		if a.ID != id {
			l.Apps = append(l.Apps, a)
		}
	}
	step("Library entry", h.saveLibrary(ctx, l, "edited in MyDailyApps"))

	h.mu.Lock()
	delete(h.updates, id)
	delete(h.installErr, id)
	delete(h.syncNote, id)
	h.mu.Unlock()
	h.Rescan()
	return report, nil
}

// safeToDelete refuses folders that are not clearly the app's own.
func (h *Host) safeToDelete(dir string, custom bool) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	abs = filepath.Clean(abs)
	home, _ := os.UserHomeDir()
	for _, bad := range []string{h.Cfg.AppsDir, h.Cfg.ExeDir, h.Cfg.DataDir, home, filepath.VolumeName(abs) + string(filepath.Separator)} {
		if bad != "" && strings.EqualFold(filepath.Clean(bad), abs) {
			return fmt.Errorf("refusing to delete %s: it is not only this app's folder", abs)
		}
	}
	// A folder that contains the host's own folders is never an app folder.
	for _, inner := range []string{h.Cfg.ExeDir, h.Cfg.DataDir, h.Cfg.AppsDir} {
		if rel, err := filepath.Rel(abs, inner); err == nil && !strings.HasPrefix(rel, "..") {
			return fmt.Errorf("refusing to delete %s: it contains MyDailyApps' own files", abs)
		}
	}
	if !custom {
		rel, err := filepath.Rel(h.Cfg.AppsDir, abs)
		if err != nil || strings.HasPrefix(rel, "..") || rel == "." {
			return fmt.Errorf("refusing to delete %s: it is outside the apps folder", abs)
		}
	}
	return nil
}

// removeAllForce also deletes read-only files (git keeps its objects
// read-only, which makes a plain delete fail on Windows).
func removeAllForce(dir string) error {
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			_ = os.Chmod(p, 0o755)
		} else if d.Type().IsRegular() {
			_ = os.Chmod(p, 0o644)
		}
		return nil
	})
	return config.RemoveAllRetry(dir)
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
