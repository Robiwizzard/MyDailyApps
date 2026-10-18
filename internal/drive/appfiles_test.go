package drive

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/robiwizzard/mydailyapps/internal/cloudsync"
	"github.com/robiwizzard/mydailyapps/internal/drive/drivetest"
	"github.com/robiwizzard/mydailyapps/internal/library"
)

type recs map[string]cloudsync.Record

func (m recs) SyncRecords(string) (map[string]cloudsync.Record, error) {
	out := map[string]cloudsync.Record{}
	for k, v := range m {
		out[k] = v
	}
	return out, nil
}
func (m recs) PutSyncRecord(_ string, r cloudsync.Record) error { m[r.Path] = r; return nil }
func (m recs) DeleteSyncRecord(_, p string) error                { delete(m, p); return nil }

func TestAppSyncThroughDrive(t *testing.T) {
	fd, srv := drivetest.New()
	defer srv.Close()
	APIBase, UploadBase = fd.URL, fd.URL+"/upload"
	tok := &oauth2.Token{AccessToken: "tok", Expiry: time.Now().Add(time.Hour)}
	spec := &library.Cloud{Include: []string{"**"}, SQLite: []string{"app.db"}}
	ctx := context.Background()

	mk := func() (*cloudsync.Syncer, string) {
		svc := New("client", "secret", "http://127.0.0.1/cb", &memStore{t: tok})
		return &cloudsync.Syncer{Remote: svc, Records: recs{}}, t.TempDir()
	}
	a, aDir := mk()
	b, bDir := mk()
	old := time.Now().Add(-time.Hour)
	os.MkdirAll(filepath.Join(aDir, "img"), 0o700)
	os.WriteFile(filepath.Join(aDir, "app.db"), []byte("db1"), 0o600)
	os.WriteFile(filepath.Join(aDir, "img", "a.png"), []byte("png"), 0o600)
	os.Chtimes(filepath.Join(aDir, "app.db"), old, old)

	if rep, err := a.Push(ctx, "cookbook", aDir, spec); err != nil || len(rep.Uploaded) != 2 {
		t.Fatalf("push: %+v %v", rep, err)
	}
	if rep, err := b.Pull(ctx, "cookbook", bDir, spec); err != nil || len(rep.Downloaded) != 2 {
		t.Fatalf("pull: %+v %v", rep, err)
	}
	if got, _ := os.ReadFile(filepath.Join(bDir, "img", "a.png")); string(got) != "png" {
		t.Fatalf("got %q", got)
	}
	st, _ := os.Stat(filepath.Join(bDir, "app.db"))
	if st.ModTime().Sub(old).Abs() > time.Second {
		t.Fatalf("modification time not kept: %v vs %v", st.ModTime(), old)
	}
	// Conflict: both change, B newer and pushes; A pulls and gets B's copy, A's is backed up.
	os.WriteFile(filepath.Join(aDir, "app.db"), []byte("from A"), 0o600)
	os.Chtimes(filepath.Join(aDir, "app.db"), old.Add(time.Minute), old.Add(time.Minute))
	os.WriteFile(filepath.Join(bDir, "app.db"), []byte("from B"), 0o600)
	if rep, err := b.Push(ctx, "cookbook", bDir, spec); err != nil || len(rep.Uploaded) != 1 {
		t.Fatalf("push B: %+v %v", rep, err)
	}
	rep, err := a.Pull(ctx, "cookbook", aDir, spec)
	if err != nil || len(rep.Conflicts) != 1 {
		t.Fatalf("pull A: %+v %v", rep, err)
	}
	if got, _ := os.ReadFile(filepath.Join(aDir, "app.db")); string(got) != "from B" {
		t.Fatalf("got %q", got)
	}
	found := false
	for _, x := range fd.Live() {
		if strings.Contains(x.Name, "(this computer) app.db") && x.Data == "from A" {
			found = true
		}
	}
	if !found {
		t.Fatal("backup of A's copy not found in Drive")
	}
}
