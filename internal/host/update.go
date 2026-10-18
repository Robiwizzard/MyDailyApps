package host

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/robiwizzard/mydailyapps/internal/install"
	"github.com/robiwizzard/mydailyapps/internal/library"
)

// CheckUpdates looks for newer versions of every installed app (git fetch or
// the newest GitHub release): "Fetch all", and the check after startup. It
// only looks; nothing is pulled until the user clicks Update. If a check is
// already running, it waits for that one instead of starting another.
func (h *Host) CheckUpdates(ctx context.Context) {
	if !h.checkMu.TryLock() {
		h.checkMu.Lock() // wait for the running check: its results are fresh
		h.checkMu.Unlock()
		return
	}
	defer h.checkMu.Unlock()
	h.mu.Lock()
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
			h.CheckUpdate(ctx, id)
		}(e.ID)
	}
	wg.Wait()
}

// CheckUpdate looks for a newer version of one installed app (the card's
// Fetch button). A failed check (offline, private repo) is shown on the card
// and keeps the earlier result.
func (h *Host) CheckUpdate(ctx context.Context, id string) (install.UpdateStatus, error) {
	a, err := h.app(id)
	if err != nil {
		return install.UpdateStatus{}, err
	}
	s, _ := h.Store.App(id)
	dir := install.Folder(h.Cfg.AppsDir, s.InstallPath, id)
	if !install.Scan(a, dir).Installed {
		return install.UpdateStatus{}, errors.New("the app is not installed")
	}
	h.mu.Lock()
	if h.fetching[id] || h.updating[id] || h.installing[id] {
		h.mu.Unlock()
		return h.updates[id], errors.New("already busy with this app")
	}
	h.fetching[id] = true
	h.mu.Unlock()
	h.bump()

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	st, err := install.CheckUpdate(ctx, a, dir, h.tokenFor(a.Repo, ""))
	h.mu.Lock()
	delete(h.fetching, id)
	if err != nil {
		log.Printf("update check %s: %v", id, err)
		h.fetchErr[id] = "Could not check for updates: " + err.Error()
		st = h.updates[id]
	} else {
		delete(h.fetchErr, id)
		h.updates[id] = st
	}
	h.mu.Unlock()
	h.bump()
	return st, err
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
