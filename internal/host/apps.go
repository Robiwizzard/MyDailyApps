package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/robiwizzard/mydailyapps/internal/install"
	"github.com/robiwizzard/mydailyapps/internal/library"
)

// Manifests: an installed app's own mydailyapps.json wins. Before an app is
// installed, a copy fetched from its GitHub repository is cached in
// <data dir>/manifests/<id>.json.

func (h *Host) manifestCachePath(id string) string {
	return filepath.Join(h.Cfg.DataDir, "manifests", id+".json")
}

// manifestFor returns the app's manifest, or nil if none is known.
func (h *Host) manifestFor(id, dir string) *library.App {
	if m, err := install.ReadManifest(dir); err == nil && m != nil {
		return m
	} else if err != nil {
		h.setInstallNote(id, "The app's "+library.ManifestName+" is invalid: "+err.Error())
	}
	b, err := os.ReadFile(h.manifestCachePath(id))
	if err != nil {
		return nil
	}
	m, err := library.ParseManifest(b)
	if err != nil {
		return nil
	}
	return m
}

func (h *Host) manifestMerged(id, dir string) library.App {
	l, _ := h.Lib.Get()
	e, _ := l.Find(id)
	return library.Merge(e, h.manifestFor(id, dir))
}

func (h *Host) setInstallNote(id, msg string) {
	h.mu.Lock()
	h.installErr[id] = msg
	h.mu.Unlock()
}

// fetchManifest downloads the manifest from the app's repository and caches it.
func (h *Host) fetchManifest(ctx context.Context, a library.App, token string) error {
	if !strings.Contains(a.Repo, "github.com/") {
		return nil
	}
	b, err := install.FetchManifest(ctx, a.Repo, token)
	if err != nil {
		return err
	}
	if _, err := library.ParseManifest(b); err != nil {
		return fmt.Errorf("the repository's %s is invalid: %w", library.ManifestName, err)
	}
	p := h.manifestCachePath(a.ID)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

// RefreshManifests fetches manifests for apps that are not installed (an
// installed app's own file is read from its folder). Returns a summary.
func (h *Host) RefreshManifests(ctx context.Context) string {
	l, _ := h.Lib.Get()
	states, _ := h.Store.Apps()
	var mu sync.Mutex
	var wg sync.WaitGroup
	fetched, failed := 0, 0
	sem := make(chan struct{}, 4)
	for _, e := range l.Apps {
		dir := h.folder(e.ID, states)
		if m, _ := install.ReadManifest(dir); m != nil || e.Repo == "" {
			continue
		}
		wg.Add(1)
		go func(e library.App) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			err := h.fetchManifest(ctx, e, h.tokenFor(e.Repo, ""))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				fetched++
			case errors.Is(err, install.ErrNoManifest):
			default:
				failed++
			}
		}(e)
	}
	wg.Wait()
	switch {
	case failed > 0:
		return fmt.Sprintf("%d read from GitHub, %d could not be fetched (using cached copies)", fetched, failed)
	case fetched > 0:
		return fmt.Sprintf("%d read from GitHub", fetched)
	default:
		return "nothing to fetch"
	}
}

// cloudList describes the synced files for the confirmation dialog.
func cloudList(c *library.Cloud) []string {
	if c == nil {
		return nil
	}
	out := append([]string{}, c.Include...)
	for _, x := range c.Exclude {
		out = append(out, "except "+x)
	}
	return out
}

func copyBools(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func copyStrings(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
