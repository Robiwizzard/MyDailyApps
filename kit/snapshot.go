package kit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Snapshots keeps timestamped copies of a file (for example the app's SQLite
// database) in a Storage, pruning to the newest Keep copies.
//
// Names look like "<Prefix>20261017T150405Z<Ext>", so they sort by time.
type Snapshots struct {
	Store  Storage
	Prefix string // e.g. "cookbook-"
	Ext    string // e.g. ".db"
	Keep   int    // how many to keep (default 5)
}

const stampLayout = "20060102T150405Z"

func (s *Snapshots) keep() int {
	if s.Keep <= 0 {
		return 5
	}
	return s.Keep
}

// Save stores data as a new snapshot and deletes the oldest beyond Keep.
func (s *Snapshots) Save(ctx context.Context, data []byte) (string, error) {
	name := s.Prefix + time.Now().UTC().Format(stampLayout) + s.Ext
	if err := s.Store.Put(ctx, name, data); err != nil {
		return "", err
	}
	list, err := s.list(ctx)
	if err != nil {
		return name, err
	}
	for i := s.keep(); i < len(list); i++ {
		if err := s.Store.Delete(ctx, list[i].name); err != nil {
			return name, err
		}
	}
	return name, nil
}

// SaveFile snapshots a local file.
func (s *Snapshots) SaveFile(ctx context.Context, path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return s.Save(ctx, b)
}

type snap struct {
	name string
	at   time.Time
}

// list returns snapshots newest first, by the time in their name.
func (s *Snapshots) list(ctx context.Context) ([]snap, error) {
	objs, err := s.Store.List(ctx, s.Prefix)
	if err != nil {
		return nil, err
	}
	var out []snap
	for _, o := range objs {
		stamp := strings.TrimSuffix(strings.TrimPrefix(o.Name, s.Prefix), s.Ext)
		t, err := time.Parse(stampLayout, stamp)
		if err != nil {
			continue
		}
		out = append(out, snap{o.Name, t})
	}
	for i := 1; i < len(out); i++ { // insertion sort, lists are short
		for j := i; j > 0 && out[j].at.After(out[j-1].at); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

// Latest returns the newest snapshot's name and time.
func (s *Snapshots) Latest(ctx context.Context) (string, time.Time, error) {
	l, err := s.list(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	if len(l) == 0 {
		return "", time.Time{}, ErrNotExist
	}
	return l[0].name, l[0].at, nil
}

// RestoreIfNewer is the startup check: if the newest snapshot is newer than
// the local file (or the file does not exist), it is written to path.
// Returns true if the local file was replaced.
func (s *Snapshots) RestoreIfNewer(ctx context.Context, path string) (bool, error) {
	name, at, err := s.Latest(ctx)
	if errors.Is(err, ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if st, err := os.Stat(path); err == nil && !at.After(st.ModTime().UTC().Add(time.Second)) {
		return false, nil
	}
	b, err := s.Store.Get(ctx, name)
	if err != nil {
		return false, fmt.Errorf("download %s: %w", name, err)
	}
	tmp := path + ".restore"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return false, err
	}
	_ = os.Chtimes(path, at, at)
	return true, nil
}
