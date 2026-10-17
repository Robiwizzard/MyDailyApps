// Package cloudsync keeps an app's data files in Google Drive, the way Steam
// Auto-Cloud does: newer cloud files are downloaded before the app starts and
// changed files are uploaded after it exits. The app itself never talks to
// Google.
//
// Conflicts (a file changed both here and in the cloud since the last sync)
// are resolved without asking: the newer copy wins and the other copy is
// saved to the app's backup folder in Drive.
//
// Deleting a file is not synced yet: a file deleted on one computer comes
// back from the cloud on the next pull.
package cloudsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/robiwizzard/mydailyapps/internal/library"
)

// RemoteFile is one synced file in the cloud.
type RemoteFile struct {
	ID       string
	Path     string // relative to DATA_DIR, forward slashes
	SHA256   string
	Modified time.Time // modification time of the file when it was uploaded
	Size     int64
}

// Remote is the cloud side. internal/drive implements it.
type Remote interface {
	// List returns the files stored for an app.
	List(ctx context.Context, appID string) ([]RemoteFile, error)
	// Download writes a file's content to w.
	Download(ctx context.Context, fileID string, w io.Writer) error
	// Upload creates (existingID == "") or replaces a file.
	Upload(ctx context.Context, appID, existingID string, f RemoteFile, r io.Reader) (RemoteFile, error)
	// BackupLocal stores a local copy in the app's backup folder.
	BackupLocal(ctx context.Context, appID, name string, r io.Reader) error
	// BackupRemote copies a cloud file into the app's backup folder.
	BackupRemote(ctx context.Context, appID, fileID, name string) error
}

// Record is the version of a file at its last successful sync.
type Record struct {
	Path     string
	SHA256   string
	RemoteID string
}

// Records stores sync records locally.
type Records interface {
	SyncRecords(appID string) (map[string]Record, error)
	PutSyncRecord(appID string, r Record) error
}

// Report describes what one sync did.
type Report struct {
	Downloaded []string
	Uploaded   []string
	Conflicts  []string // "path: kept this computer's copy" etc.
	Skipped    string   // why nothing was uploaded, if so
}

func (r Report) String() string {
	var parts []string
	if n := len(r.Downloaded); n > 0 {
		parts = append(parts, fmt.Sprintf("%d downloaded", n))
	}
	if n := len(r.Uploaded); n > 0 {
		parts = append(parts, fmt.Sprintf("%d uploaded", n))
	}
	if n := len(r.Conflicts); n > 0 {
		parts = append(parts, fmt.Sprintf("%d conflict(s) resolved, older copy backed up", n))
	}
	if r.Skipped != "" {
		parts = append(parts, r.Skipped)
	}
	if len(parts) == 0 {
		return "up to date"
	}
	return strings.Join(parts, ", ")
}

// Syncer syncs app data folders.
type Syncer struct {
	Remote  Remote
	Records Records
	// Checkpoint merges a SQLite database's write-ahead log into the main
	// file. Called for every listed database before upload.
	Checkpoint func(path string) error
	Now        func() time.Time
}

func (s *Syncer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

type localFile struct {
	path    string // relative, forward slashes
	abs     string
	sha     string
	modTime time.Time
	size    int64
}

// Pull downloads cloud files that are newer than this computer's copy.
// Run it only while the app is not running.
func (s *Syncer) Pull(ctx context.Context, appID, dataDir string, spec *library.Cloud) (Report, error) {
	var rep Report
	remote, err := s.Remote.List(ctx, appID)
	if err != nil {
		return rep, err
	}
	recs, err := s.Records.SyncRecords(appID)
	if err != nil {
		return rep, err
	}
	for _, r := range remote {
		if !Matches(spec, r.Path) {
			continue
		}
		local, err := hashLocal(dataDir, r.Path)
		if err != nil {
			return rep, err
		}
		rec, hasRec := recs[r.Path]
		switch {
		case local != nil && local.sha == r.SHA256:
			// Same content on both sides.
		case local == nil:
			if err := s.download(ctx, dataDir, r, spec); err != nil {
				return rep, err
			}
			rep.Downloaded = append(rep.Downloaded, r.Path)
		case hasRec && rec.SHA256 == r.SHA256:
			// Cloud unchanged since the last sync; a local change is uploaded by Push.
			continue
		case hasRec && rec.SHA256 == local.sha:
			// Only the cloud changed.
			if err := s.download(ctx, dataDir, r, spec); err != nil {
				return rep, err
			}
			rep.Downloaded = append(rep.Downloaded, r.Path)
		default:
			// Both changed (or never synced and different): the newer copy wins.
			if r.Modified.After(local.modTime) {
				if err := s.backupLocal(ctx, appID, local); err != nil {
					return rep, err
				}
				if err := s.download(ctx, dataDir, r, spec); err != nil {
					return rep, err
				}
				rep.Downloaded = append(rep.Downloaded, r.Path)
				rep.Conflicts = append(rep.Conflicts, r.Path+": the cloud copy was newer; this computer's copy was backed up")
			} else {
				if err := s.Remote.BackupRemote(ctx, appID, r.ID, backupName(r.Path, "cloud", s.now())); err != nil {
					return rep, fmt.Errorf("backing up the cloud copy of %s: %w", r.Path, err)
				}
				rep.Conflicts = append(rep.Conflicts, r.Path+": this computer's copy was newer; the cloud copy was backed up")
				// Treat the cloud copy as seen so Push uploads the local one.
				if err := s.Records.PutSyncRecord(appID, Record{Path: r.Path, SHA256: r.SHA256, RemoteID: r.ID}); err != nil {
					return rep, err
				}
				continue
			}
		}
		if err := s.Records.PutSyncRecord(appID, Record{Path: r.Path, SHA256: r.SHA256, RemoteID: r.ID}); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// Push uploads files that changed on this computer. Run it after the app exited.
func (s *Syncer) Push(ctx context.Context, appID, dataDir string, spec *library.Cloud) (Report, error) {
	var rep Report
	if s.Checkpoint != nil {
		for _, db := range spec.SQLite {
			p := filepath.Join(dataDir, filepath.FromSlash(db))
			if _, err := os.Stat(p); err == nil {
				if err := s.Checkpoint(p); err != nil {
					return rep, fmt.Errorf("preparing database %s for upload: %w", db, err)
				}
			}
		}
	}
	locals, err := scanLocal(dataDir, spec)
	if err != nil {
		return rep, err
	}
	var total int64
	for _, l := range locals {
		total += l.size
	}
	if spec.MaxSizeMB > 0 && total > int64(spec.MaxSizeMB)<<20 {
		rep.Skipped = fmt.Sprintf("not uploaded: %d MB is over the app's %d MB limit", total>>20, spec.MaxSizeMB)
		return rep, nil
	}
	remoteList, err := s.Remote.List(ctx, appID)
	if err != nil {
		return rep, err
	}
	remote := map[string]RemoteFile{}
	for _, r := range remoteList {
		remote[r.Path] = r
	}
	recs, err := s.Records.SyncRecords(appID)
	if err != nil {
		return rep, err
	}
	for _, l := range locals {
		r, inCloud := remote[l.path]
		rec, hasRec := recs[l.path]
		if inCloud && r.SHA256 == l.sha {
			if !hasRec || rec.SHA256 != l.sha {
				if err := s.Records.PutSyncRecord(appID, Record{Path: l.path, SHA256: l.sha, RemoteID: r.ID}); err != nil {
					return rep, err
				}
			}
			continue
		}
		if hasRec && rec.SHA256 == l.sha {
			continue // unchanged here; a newer cloud copy is pulled before the next start
		}
		cloudChanged := inCloud && (!hasRec || rec.SHA256 != r.SHA256)
		if cloudChanged {
			if !l.modTime.After(r.Modified) {
				// The cloud copy is newer: keep it, back up ours.
				if err := s.backupLocal(ctx, appID, &l); err != nil {
					return rep, err
				}
				rep.Conflicts = append(rep.Conflicts, l.path+": the cloud copy was newer; this computer's copy was backed up")
				// Mark ours as synced so the next Pull replaces it with the cloud copy.
				if err := s.Records.PutSyncRecord(appID, Record{Path: l.path, SHA256: l.sha, RemoteID: r.ID}); err != nil {
					return rep, err
				}
				continue
			}
			if err := s.Remote.BackupRemote(ctx, appID, r.ID, backupName(l.path, "cloud", s.now())); err != nil {
				return rep, fmt.Errorf("backing up the cloud copy of %s: %w", l.path, err)
			}
			rep.Conflicts = append(rep.Conflicts, l.path+": this computer's copy was newer; the cloud copy was backed up")
		}
		existing := ""
		if inCloud {
			existing = r.ID
		}
		f, err := os.Open(l.abs)
		if err != nil {
			return rep, err
		}
		up, err := s.Remote.Upload(ctx, appID, existing, RemoteFile{Path: l.path, SHA256: l.sha, Modified: l.modTime, Size: l.size}, f)
		f.Close()
		if err != nil {
			return rep, fmt.Errorf("uploading %s: %w", l.path, err)
		}
		if err := s.Records.PutSyncRecord(appID, Record{Path: l.path, SHA256: l.sha, RemoteID: up.ID}); err != nil {
			return rep, err
		}
		rep.Uploaded = append(rep.Uploaded, l.path)
	}
	return rep, nil
}

func (s *Syncer) backupLocal(ctx context.Context, appID string, l *localFile) error {
	f, err := os.Open(l.abs)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := s.Remote.BackupLocal(ctx, appID, backupName(l.path, "this computer", s.now()), f); err != nil {
		return fmt.Errorf("backing up this computer's copy of %s: %w", l.path, err)
	}
	return nil
}

func backupName(p, origin string, t time.Time) string {
	return fmt.Sprintf("%s (%s) %s", t.UTC().Format("2006-01-02 15-04-05"), origin, p)
}

// download writes the cloud copy into DATA_DIR atomically.
func (s *Syncer) download(ctx context.Context, dataDir string, r RemoteFile, spec *library.Cloud) error {
	dst := filepath.Join(dataDir, filepath.FromSlash(r.Path))
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := s.Remote.Download(ctx, r.ID, tmp); err != nil {
		tmp.Close()
		return fmt.Errorf("downloading %s: %w", r.Path, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if isSQLite(spec, r.Path) {
		// Stale journal files next to a replaced database would corrupt it.
		for _, ext := range []string{"-wal", "-shm", "-journal"} {
			_ = os.Remove(dst + ext)
		}
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return err
	}
	if !r.Modified.IsZero() {
		_ = os.Chtimes(dst, r.Modified, r.Modified)
	}
	return nil
}

func isSQLite(spec *library.Cloud, p string) bool {
	for _, db := range spec.SQLite {
		if db == p {
			return true
		}
	}
	return false
}

func hashLocal(dataDir, rel string) (*localFile, error) {
	abs := filepath.Join(dataDir, filepath.FromSlash(rel))
	st, err := os.Stat(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		return nil, nil
	}
	sum, err := fileSHA(abs)
	if err != nil {
		return nil, err
	}
	return &localFile{path: rel, abs: abs, sha: sum, modTime: st.ModTime(), size: st.Size()}, nil
}

func fileSHA(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func scanLocal(dataDir string, spec *library.Cloud) ([]localFile, error) {
	var out []localFile
	err := filepath.WalkDir(dataDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		rel, err := filepath.Rel(dataDir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !Matches(spec, rel) {
			return nil
		}
		l, err := hashLocal(dataDir, rel)
		if err != nil || l == nil {
			return err
		}
		out = append(out, *l)
		return nil
	})
	return out, err
}

// Matches reports whether a relative path is synced under the spec.
// SQLite side files and the host's own temp files are never synced.
func Matches(spec *library.Cloud, rel string) bool {
	if spec == nil {
		return false
	}
	base := path.Base(rel)
	if strings.HasPrefix(base, ".download-") || base == ".mydailyapps-installed.json" {
		return false
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".git" || seg == "node_modules" {
			return false // never sync code or dependencies, even with root "app"
		}
	}
	for _, ext := range []string{"-wal", "-shm", "-journal"} {
		if strings.HasSuffix(rel, ext) {
			return false
		}
	}
	in := false
	for _, p := range spec.Include {
		if matchGlob(p, rel) {
			in = true
			break
		}
	}
	if !in {
		return false
	}
	for _, p := range spec.Exclude {
		if matchGlob(p, rel) {
			return false
		}
	}
	return true
}

// matchGlob matches forward-slash paths: * within one segment, ** any depth.
// A pattern without a slash also matches that name at the top level only.
func matchGlob(pattern, name string) bool {
	return matchSegs(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegs(p, n []string) bool {
	for len(p) > 0 {
		if p[0] == "**" {
			if len(p) == 1 {
				return true
			}
			for i := 0; i <= len(n); i++ {
				if matchSegs(p[1:], n[i:]) {
					return true
				}
			}
			return false
		}
		if len(n) == 0 {
			return false
		}
		ok, err := path.Match(p[0], n[0])
		if err != nil || !ok {
			return false
		}
		p, n = p[1:], n[1:]
	}
	return len(n) == 0
}
