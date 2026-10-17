package host

import (
	"context"
	"errors"
	"log"
	"os"
	"sync"
	"time"

	"github.com/robiwizzard/mydailyapps/internal/cloudsync"
	"github.com/robiwizzard/mydailyapps/internal/install"
	"github.com/robiwizzard/mydailyapps/internal/library"
	"github.com/robiwizzard/mydailyapps/internal/state"
)

type direction int

const (
	pull direction = iota
	push
)

func (h *Host) syncLock(id string) *sync.Mutex {
	h.mu.Lock()
	defer h.mu.Unlock()
	l := h.syncLocks[id]
	if l == nil {
		l = &sync.Mutex{}
		h.syncLocks[id] = l
	}
	return l
}

// syncApp runs one pull or push. Problems are shown on the card; they never
// stop the app from starting (the local copy is used).
func (h *Host) syncApp(ctx context.Context, id, dataDir string, spec *library.Cloud, dir direction) {
	if spec == nil || !h.Drive.Connected() {
		return
	}
	lock := h.syncLock(id)
	lock.Lock()
	defer lock.Unlock()
	h.mu.Lock()
	h.syncing[id] = true
	h.mu.Unlock()
	h.bump()

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var rep cloudsync.Report
	var err error
	if dir == pull {
		rep, err = h.Sync.Pull(ctx, id, dataDir, spec)
	} else {
		rep, err = h.Sync.Push(ctx, id, dataDir, spec)
	}
	note := "Synced: " + rep.String()
	if err != nil {
		note = "Sync failed (the local copy is kept): " + err.Error()
		log.Printf("sync %s: %v", id, err)
	} else if len(rep.Conflicts) > 0 {
		note = "Synced. Older copies were saved in Drive under MyDailyApps/backups/" + id + ": " + rep.String()
	}
	h.mu.Lock()
	delete(h.syncing, id)
	h.syncNote[id] = note
	h.mu.Unlock()
	h.bump()
}

// syncAppAsync is used after an app exits; WaitSyncs waits for it at shutdown.
func (h *Host) syncAppAsync(id, dataDir string, spec *library.Cloud, dir direction) {
	if spec == nil || !h.Drive.Connected() {
		return
	}
	h.syncWG.Add(1)
	go func() {
		defer h.syncWG.Done()
		h.syncApp(context.Background(), id, dataDir, spec, dir)
	}()
}

// SyncNow pulls and then pushes a stopped app's data (the "Sync now" button).
func (h *Host) SyncNow(ctx context.Context, id string) error {
	a, err := h.app(id)
	if err != nil {
		return err
	}
	if a.Cloud == nil {
		return errors.New("this app does not sync any files")
	}
	if !h.Drive.Connected() {
		return errors.New("Google Drive is not connected")
	}
	if st, _ := h.Procs.Status(id); st != "" {
		return errors.New("stop the app first: files are synced only while it is not running")
	}
	dataDir := h.Cfg.AppDataDir(id)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	s, _ := h.Store.App(id)
	dir := h.syncRoot(id, install.Folder(h.Cfg.AppsDir, s.InstallPath, id), a.Cloud)
	h.syncApp(ctx, id, dir, a.Cloud, pull)
	h.syncApp(ctx, id, dir, a.Cloud, push)
	return nil
}

// syncRoot is the folder the cloud patterns are relative to.
func (h *Host) syncRoot(id, appDir string, spec *library.Cloud) string {
	if spec.AppRoot() {
		return appDir
	}
	return h.Cfg.AppDataDir(id)
}

// WaitSyncs waits for uploads started after apps exited, up to timeout.
func (h *Host) WaitSyncs(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() { h.syncWG.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// syncRecords adapts the local state store to cloudsync.Records.
type syncRecords struct{ s *state.Store }

func (r syncRecords) SyncRecords(appID string) (map[string]cloudsync.Record, error) {
	in, err := r.s.SyncRecords(appID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]cloudsync.Record, len(in))
	for k, v := range in {
		out[k] = cloudsync.Record{Path: v.Path, SHA256: v.SHA256, RemoteID: v.RemoteID}
	}
	return out, nil
}

func (r syncRecords) PutSyncRecord(appID string, rec cloudsync.Record) error {
	return r.s.PutSyncRecord(appID, state.SyncRecord{Path: rec.Path, SHA256: rec.SHA256, RemoteID: rec.RemoteID})
}
