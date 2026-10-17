package kit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSnapshotsKeepAndRestore(t *testing.T) {
	ctx := context.Background()
	store := LocalStorage{Dir: t.TempDir()}
	s := &Snapshots{Store: store, Prefix: "app-", Ext: ".db", Keep: 2}
	for i := 0; i < 3; i++ {
		if _, err := s.Save(ctx, []byte{byte('a' + i)}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1100 * time.Millisecond) // names have second resolution
	}
	objs, _ := store.List(ctx, "app-")
	if len(objs) != 2 {
		t.Fatalf("kept %d snapshots, want 2", len(objs))
	}
	local := filepath.Join(t.TempDir(), "app.db")
	restored, err := s.RestoreIfNewer(ctx, local)
	if err != nil || !restored {
		t.Fatalf("restore: %v %v", restored, err)
	}
	b, _ := os.ReadFile(local)
	if string(b) != "c" {
		t.Fatalf("restored %q, want newest", b)
	}
	if again, _ := s.RestoreIfNewer(ctx, local); again {
		t.Fatal("should not restore when local is up to date")
	}
}

func TestLoadGuest(t *testing.T) {
	t.Setenv("PORT", "")
	t.Setenv("DATA_DIR", "")
	t.Setenv("MYDAILYAPPS_MODE", "")
	c, err := Load("demo", 8099)
	if err != nil || c.Port != 8099 || !c.Guest {
		t.Fatalf("%+v %v", c, err)
	}
	c.Cleanup()
	if _, err := os.Stat(c.DataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("guest data dir not removed")
	}
	t.Setenv("PORT", "9123")
	t.Setenv("DATA_DIR", t.TempDir())
	c, _ = Load("demo", 8099)
	if c.Port != 9123 || c.Guest {
		t.Fatalf("%+v", c)
	}
}
