package drive

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/robiwizzard/mydailyapps/internal/cloudsync"
	"github.com/robiwizzard/mydailyapps/internal/library"
)

type fakeFileRec struct {
	id, name, parent, mime, data string
	props                        map[string]string
	modified                     time.Time
}

// fakeDrive2 implements the Drive calls used for app sync, including
// resumable uploads and server-side copies.
type fakeDrive2 struct {
	mu       sync.Mutex
	files    map[string]*fakeFileRec
	sessions map[string]*fakeFileRec // pending upload sessions
	n        int
	srv      *httptest.Server
}

var (
	reName   = regexp.MustCompile(`name = '([^']*)'`)
	reParent = regexp.MustCompile(`'([^']*)' in parents`)
)

func (f *fakeDrive2) id() string { f.n++; return fmt.Sprint("f", f.n) }

func (f *fakeDrive2) meta(x *fakeFileRec) map[string]any {
	return map[string]any{"id": x.id, "name": x.name, "size": fmt.Sprint(len(x.data)),
		"modifiedTime": x.modified.Format(time.RFC3339Nano), "appProperties": x.props}
}

func (f *fakeDrive2) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	switch {
	case r.Method == "GET" && p == "/files":
		q := r.URL.Query().Get("q")
		parent := ""
		if m := reParent.FindStringSubmatch(q); m != nil {
			parent = m[1]
		}
		name := ""
		if m := reName.FindStringSubmatch(q); m != nil {
			name = m[1]
		}
		var out []map[string]any
		for _, x := range f.files {
			if parent != "" && parent != "root" && x.parent != parent {
				continue
			}
			if parent == "root" && x.parent != "" {
				continue
			}
			if name != "" && x.name != name {
				continue
			}
			if strings.Contains(q, "mimeType = 'application/vnd.google-apps.folder'") && x.mime != folderMime {
				continue
			}
			if strings.Contains(q, "mimeType != 'application/vnd.google-apps.folder'") && x.mime == folderMime {
				continue
			}
			out = append(out, f.meta(x))
		}
		json.NewEncoder(w).Encode(map[string]any{"files": out})
	case r.Method == "POST" && p == "/files":
		var m struct {
			Name, MimeType string
			Parents        []string
		}
		json.NewDecoder(r.Body).Decode(&m)
		x := &fakeFileRec{id: f.id(), name: m.Name, mime: m.MimeType, modified: time.Now()}
		if len(m.Parents) > 0 {
			x.parent = m.Parents[0]
		}
		f.files[x.id] = x
		json.NewEncoder(w).Encode(map[string]string{"id": x.id})
	case (r.Method == "POST" && p == "/upload/files") || (r.Method == "PATCH" && strings.HasPrefix(p, "/upload/files/")):
		var m struct {
			Name          string
			Parents       []string
			AppProperties map[string]string
		}
		json.NewDecoder(r.Body).Decode(&m)
		var x *fakeFileRec
		if r.Method == "PATCH" {
			old := f.files[strings.TrimPrefix(p, "/upload/files/")]
			cp := *old
			x = &cp
		} else {
			x = &fakeFileRec{id: f.id(), name: m.Name}
			if len(m.Parents) > 0 {
				x.parent = m.Parents[0]
			}
		}
		if m.AppProperties != nil {
			x.props = m.AppProperties
		}
		sid := f.id()
		f.sessions[sid] = x
		w.Header().Set("Location", f.srv.URL+"/session/"+sid)
	case r.Method == "PUT" && strings.HasPrefix(p, "/session/"):
		x := f.sessions[strings.TrimPrefix(p, "/session/")]
		b, _ := io.ReadAll(r.Body)
		if r.ContentLength != int64(len(b)) {
			http.Error(w, "length", 400)
			return
		}
		x.data, x.modified = string(b), time.Now()
		f.files[x.id] = x
		json.NewEncoder(w).Encode(f.meta(x))
	case r.Method == "GET" && strings.HasPrefix(p, "/files/"):
		x := f.files[strings.TrimPrefix(p, "/files/")]
		io.WriteString(w, x.data)
	case r.Method == "POST" && strings.HasSuffix(p, "/copy"):
		src := f.files[strings.TrimSuffix(strings.TrimPrefix(p, "/files/"), "/copy")]
		var m struct {
			Name    string
			Parents []string
		}
		json.NewDecoder(r.Body).Decode(&m)
		x := &fakeFileRec{id: f.id(), name: m.Name, parent: m.Parents[0], data: src.data, modified: time.Now()}
		f.files[x.id] = x
		json.NewEncoder(w).Encode(map[string]string{"id": x.id})
	default:
		http.Error(w, "unexpected "+r.Method+" "+p, 400)
	}
}

type recs map[string]cloudsync.Record

func (m recs) SyncRecords(string) (map[string]cloudsync.Record, error) {
	out := map[string]cloudsync.Record{}
	for k, v := range m {
		out[k] = v
	}
	return out, nil
}
func (m recs) PutSyncRecord(_ string, r cloudsync.Record) error { m[r.Path] = r; return nil }

func TestAppSyncThroughDrive(t *testing.T) {
	fd := &fakeDrive2{files: map[string]*fakeFileRec{}, sessions: map[string]*fakeFileRec{}}
	fd.srv = httptest.NewServer(fd)
	defer fd.srv.Close()
	APIBase, UploadBase = fd.srv.URL, fd.srv.URL+"/upload"
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
	for _, x := range fd.files {
		if strings.Contains(x.name, "(this computer) app.db") && x.data == "from A" {
			found = true
		}
	}
	if !found {
		t.Fatal("backup of A's copy not found in Drive")
	}
}
