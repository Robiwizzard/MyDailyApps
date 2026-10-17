package cloudsync

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robiwizzard/mydailyapps/internal/library"
)

// fakeRemote is an in-memory cloud shared by several "computers".
type fakeRemote struct {
	files   map[string]*fakeFile // by id
	backups map[string]string    // name -> content
	n       int
}

type fakeFile struct {
	RemoteFile
	data string
}

func newFakeRemote() *fakeRemote {
	return &fakeRemote{files: map[string]*fakeFile{}, backups: map[string]string{}}
}

func (f *fakeRemote) List(ctx context.Context, appID string) ([]RemoteFile, error) {
	var out []RemoteFile
	for _, x := range f.files {
		out = append(out, x.RemoteFile)
	}
	return out, nil
}

func (f *fakeRemote) Download(ctx context.Context, id string, w io.Writer) error {
	_, err := io.WriteString(w, f.files[id].data)
	return err
}

func (f *fakeRemote) Upload(ctx context.Context, appID, existing string, m RemoteFile, r io.Reader) (RemoteFile, error) {
	b, _ := io.ReadAll(r)
	id := existing
	if id == "" {
		f.n++
		id = fmt.Sprint("id", f.n)
	}
	m.ID = id
	f.files[id] = &fakeFile{RemoteFile: m, data: string(b)}
	return m, nil
}

func (f *fakeRemote) BackupLocal(ctx context.Context, appID, name string, r io.Reader) error {
	b, _ := io.ReadAll(r)
	f.backups[name] = string(b)
	return nil
}

func (f *fakeRemote) BackupRemote(ctx context.Context, appID, id, name string) error {
	f.backups[name] = f.files[id].data
	return nil
}

type memRecords map[string]Record

func (m memRecords) SyncRecords(string) (map[string]Record, error) {
	out := map[string]Record{}
	for k, v := range m {
		out[k] = v
	}
	return out, nil
}
func (m memRecords) PutSyncRecord(_ string, r Record) error { m[r.Path] = r; return nil }

type computer struct {
	dir string
	s   *Syncer
}

func newComputer(t *testing.T, r Remote) *computer {
	return &computer{dir: t.TempDir(), s: &Syncer{Remote: r, Records: memRecords{}}}
}

func (c *computer) write(t *testing.T, rel, content string, mod time.Time) {
	p := filepath.Join(c.dir, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(p), 0o700)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(p, mod, mod)
}

func (c *computer) read(rel string) string {
	b, _ := os.ReadFile(filepath.Join(c.dir, filepath.FromSlash(rel)))
	return string(b)
}

var spec = &library.Cloud{Include: []string{"app.db", "images/**"}, Exclude: []string{"images/cache/**"}, SQLite: []string{"app.db"}}

func TestRoundTripBetweenComputers(t *testing.T) {
	ctx := context.Background()
	cloud := newFakeRemote()
	a, b := newComputer(t, cloud), newComputer(t, cloud)
	t0 := time.Now().Add(-time.Hour)

	a.write(t, "app.db", "v1", t0)
	a.write(t, "images/cat.png", "cat", t0)
	a.write(t, "images/cache/x.png", "cache", t0)
	a.write(t, "app.db-wal", "wal", t0)
	a.write(t, "notes.txt", "not synced", t0)
	rep, err := a.s.Push(ctx, "x", a.dir, spec)
	if err != nil || len(rep.Uploaded) != 2 {
		t.Fatalf("push A: %+v %v", rep, err)
	}

	b.write(t, "app.db-wal", "stale", t0)
	rep, err = b.s.Pull(ctx, "x", b.dir, spec)
	if err != nil || len(rep.Downloaded) != 2 || b.read("app.db") != "v1" {
		t.Fatalf("pull B: %+v %v", rep, err)
	}
	if _, err := os.Stat(filepath.Join(b.dir, "app.db-wal")); !os.IsNotExist(err) {
		t.Fatal("stale WAL file should be removed when the database is replaced")
	}

	// B changes the database; A gets it on its next pull.
	b.write(t, "app.db", "v2", t0.Add(10*time.Minute))
	if rep, _ := b.s.Push(ctx, "x", b.dir, spec); len(rep.Uploaded) != 1 {
		t.Fatalf("push B: %+v", rep)
	}
	if rep, _ := a.s.Pull(ctx, "x", a.dir, spec); len(rep.Downloaded) != 1 || a.read("app.db") != "v2" {
		t.Fatalf("pull A: %+v %q", rep, a.read("app.db"))
	}
	// Nothing to do the second time.
	if rep, _ := a.s.Pull(ctx, "x", a.dir, spec); rep.String() != "up to date" {
		t.Fatalf("expected up to date, got %s", rep)
	}
}

func TestConflictNewerWinsOlderBackedUp(t *testing.T) {
	ctx := context.Background()
	cloud := newFakeRemote()
	a, b := newComputer(t, cloud), newComputer(t, cloud)
	t0 := time.Now().Add(-time.Hour)
	a.write(t, "app.db", "base", t0)
	a.s.Push(ctx, "x", a.dir, spec)
	b.s.Pull(ctx, "x", b.dir, spec)

	// Both edit offline. B is newer and pushes first.
	a.write(t, "app.db", "from A", t0.Add(5*time.Minute))
	b.write(t, "app.db", "from B", t0.Add(20*time.Minute))
	b.s.Push(ctx, "x", b.dir, spec)

	// A starts its app: the cloud (B) is newer, A's copy is backed up.
	rep, err := a.s.Pull(ctx, "x", a.dir, spec)
	if err != nil || len(rep.Conflicts) != 1 || a.read("app.db") != "from B" {
		t.Fatalf("pull: %+v %v %q", rep, err, a.read("app.db"))
	}
	if !hasBackup(cloud, "this computer", "from A") {
		t.Fatalf("A's copy not backed up: %v", cloud.backups)
	}
}

func TestConflictLocalNewerOnPush(t *testing.T) {
	ctx := context.Background()
	cloud := newFakeRemote()
	a, b := newComputer(t, cloud), newComputer(t, cloud)
	t0 := time.Now().Add(-time.Hour)
	a.write(t, "app.db", "base", t0)
	a.s.Push(ctx, "x", a.dir, spec)
	b.s.Pull(ctx, "x", b.dir, spec)

	b.write(t, "app.db", "from B", t0.Add(5*time.Minute))
	b.s.Push(ctx, "x", b.dir, spec)
	a.write(t, "app.db", "from A", t0.Add(30*time.Minute)) // A ran without pulling first

	rep, err := a.s.Push(ctx, "x", a.dir, spec)
	if err != nil || len(rep.Conflicts) != 1 || len(rep.Uploaded) != 1 {
		t.Fatalf("push: %+v %v", rep, err)
	}
	if !hasBackup(cloud, "cloud", "from B") {
		t.Fatalf("cloud copy not backed up: %v", cloud.backups)
	}
	b.s.Pull(ctx, "x", b.dir, spec)
	if b.read("app.db") != "from A" {
		t.Fatalf("B should now have A's newer copy, has %q", b.read("app.db"))
	}
}

func TestSizeLimit(t *testing.T) {
	c := newComputer(t, newFakeRemote())
	c.write(t, "app.db", strings.Repeat("x", 2<<20), time.Now())
	rep, err := c.s.Push(context.Background(), "x", c.dir, &library.Cloud{Include: []string{"*"}, MaxSizeMB: 1})
	if err != nil || rep.Skipped == "" || len(rep.Uploaded) != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
}

func TestGlob(t *testing.T) {
	cases := []struct {
		p, n string
		ok   bool
	}{
		{"app.db", "app.db", true},
		{"app.db", "sub/app.db", false},
		{"images/**", "images/a/b.png", true},
		{"images/**", "images", true},
		{"**/*.json", "a/b/c.json", true},
		{"**/*.json", "c.json", true},
		{"*.db", "x/y.db", false},
	}
	for _, c := range cases {
		if got := matchGlob(c.p, c.n); got != c.ok {
			t.Errorf("%s ~ %s: got %v", c.p, c.n, got)
		}
	}
}

func hasBackup(f *fakeRemote, origin, content string) bool {
	for name, data := range f.backups {
		if strings.Contains(name, "("+origin+")") && data == content {
			return true
		}
	}
	return false
}
