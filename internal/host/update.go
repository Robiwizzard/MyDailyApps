package host

import (
	"context"
	"errors"
	"log"
	"sync"

	"github.com/robiwizzard/mydailyapps/internal/install"
	"github.com/robiwizzard/mydailyapps/internal/library"
)

// CheckUpdates looks for newer versions of every installed app (git fetch or
// the newest GitHub release). It only looks; nothing is pulled until the
// user clicks Update. Runs in the background after startup.
func (h *Host) CheckUpdates(ctx context.Context) {
	h.mu.Lock()
	if h.checkingUpdates {
		h.mu.Unlock()
		return
	}
	h.checkingUpdates = true
	h.mu.Unlock()
	h.bump()
	defer func() {
		h.mu.Lock()
		h.checkingUpdates = false
		h.mu.Unlock()
		h.bump()
	}()

	l, _ := h.Lib.Get()
	states, _ := h.Store.Apps()
	h.mu.Lock()
	scan := h.scan
	h.mu.Unlock()
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, e := range l.Apps {
		if !scan[e.ID].Installed {
			continue
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			a, err := h.app(id)
			if err != nil {
				return
			}
			st, err := install.CheckUpdate(ctx, a, h.folder(id, states), h.tokenFor(a.Repo, ""))
			if err != nil {
				log.Printf("update check %s: %v", id, err)
				return // offline or private: just don't show an update
			}
			h.mu.Lock()
			h.updates[id] = st
			h.mu.Unlock()
			h.bump()
		}(e.ID)
	}
	wg.Wait()
}

// Update brings one stopped app to its newest version. Without confirm it
// returns a *ConfirmError listing the exact steps.
func (h *Host) Update(ctx context.Context, id string, confirm bool, token string, logf func(string)) error {
	a, err := h.app(id)
	if err != nil {
		return err
	}
	if st, _ := h.Procs.Status(id); st != "" {
		return errors.New("stop the app first, then update it")
	}
	s, _ := h.Store.App(id)
	dir := install.Folder(h.Cfg.AppsDir, s.InstallPath, id)
	if !install.Scan(a, dir).Installed {
		return errors.New("the app is not installed")
	}
	setup := currentSetup(a, dir)
	if !confirm {
		return &ConfirmError{Kind: "update", Folder: dir, Commands: install.UpdateCommands(a, dir, setup), Launch: install.Scan(a, dir).Launch}
	}
	h.mu.Lock()
	if h.updating[id] || h.installing[id] {
		h.mu.Unlock()
		return errors.New("already busy with this app")
	}
	h.updating[id] = true
	delete(h.installErr, id)
	h.mu.Unlock()
	h.bump()
	defer func() {
		h.mu.Lock()
		delete(h.updating, id)
		h.mu.Unlock()
		h.Rescan()
	}()

	tok := h.tokenFor(a.Repo, token)
	err = install.Update(ctx, a, dir, tok, func() string {
		// The update may have changed the app's manifest: use its setup command.
		return currentSetup(h.manifestMerged(id, dir), dir)
	}, logf)
	if errors.Is(err, install.ErrNeedsToken) {
		return &TokenError{Message: "The repository needs a GitHub token to update (it may be private). The token is used for this session only."}
	}
	if err != nil {
		return err
	}
	h.mu.Lock()
	delete(h.updates, id)
	h.mu.Unlock()
	return nil
}

// currentSetup is the setup command of the install option the app uses.
func currentSetup(a library.App, dir string) string {
	r := install.Scan(a, dir)
	if r.Option >= 0 && r.Option < len(a.Install) {
		return a.Install[r.Option].Setup
	}
	return ""
}
