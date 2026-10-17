package kit

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ErrNotExist is returned by Get for a missing object.
var ErrNotExist = fs.ErrNotExist

// Object is one stored file.
type Object struct {
	Name     string
	Modified time.Time
	Size     int64
}

// Storage is a flat store of named files. LocalStorage and DriveStorage
// implement it, so an app can work the same offline and with Drive.
type Storage interface {
	Put(ctx context.Context, name string, data []byte) error
	Get(ctx context.Context, name string) ([]byte, error)
	List(ctx context.Context, prefix string) ([]Object, error) // newest first
	Delete(ctx context.Context, name string) error
}

// LocalStorage keeps files in a folder.
type LocalStorage struct{ Dir string }

func (l LocalStorage) path(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return "", errors.New("invalid object name")
	}
	return filepath.Join(l.Dir, name), nil
}

func (l LocalStorage) Put(_ context.Context, name string, data []byte) error {
	p, err := l.path(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (l LocalStorage) Get(_ context.Context, name string) ([]byte, error) {
	p, err := l.path(name)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

func (l LocalStorage) List(_ context.Context, prefix string) ([]Object, error) {
	entries, err := os.ReadDir(l.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Object
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Object{Name: e.Name(), Modified: info.ModTime(), Size: info.Size()})
	}
	sortNewest(out)
	return out, nil
}

func (l LocalStorage) Delete(_ context.Context, name string) error {
	p, err := l.path(name)
	if err != nil {
		return err
	}
	err = os.Remove(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func sortNewest(o []Object) {
	sort.Slice(o, func(i, j int) bool {
		if o[i].Modified.Equal(o[j].Modified) {
			return o[i].Name > o[j].Name
		}
		return o[i].Modified.After(o[j].Modified)
	})
}
