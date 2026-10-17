package drive

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

type memStore struct{ t *oauth2.Token }

func (m *memStore) LoadToken() (*oauth2.Token, error) { return m.t, nil }
func (m *memStore) SaveToken(t *oauth2.Token) error   { m.t = t; return nil }

// A tiny fake of the Drive endpoints the host uses.
func fakeDrive(t *testing.T) *httptest.Server {
	var mu sync.Mutex
	files := map[string]struct{ name, parent, data string }{}
	n := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no auth", 401)
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/files":
			q := r.URL.Query().Get("q")
			var out []map[string]string
			for id, f := range files {
				if strings.Contains(q, "name = '"+f.name+"'") && (f.parent == "" || strings.Contains(q, f.parent)) {
					out = append(out, map[string]string{"id": id, "name": f.name})
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"files": out})
		case r.Method == "POST" && r.URL.Path == "/files": // create folder
			var m map[string]any
			json.NewDecoder(r.Body).Decode(&m)
			n++
			id := "folder" + string(rune('0'+n))
			files[id] = struct{ name, parent, data string }{m["name"].(string), "", ""}
			json.NewEncoder(w).Encode(map[string]string{"id": id})
		case r.Method == "POST" && r.URL.Path == "/upload/files":
			_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			mr := multipart.NewReader(r.Body, params["boundary"])
			p1, _ := mr.NextPart()
			var meta struct {
				Name    string   `json:"name"`
				Parents []string `json:"parents"`
			}
			json.NewDecoder(p1).Decode(&meta)
			p2, _ := mr.NextPart()
			b, _ := io.ReadAll(p2)
			n++
			files["file"+string(rune('0'+n))] = struct{ name, parent, data string }{meta.Name, meta.Parents[0], string(b)}
			w.Write([]byte("{}"))
		case r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, "/upload/files/"):
			id := strings.TrimPrefix(r.URL.Path, "/upload/files/")
			b, _ := io.ReadAll(r.Body)
			f := files[id]
			f.data = string(b)
			files[id] = f
			w.Write([]byte("{}"))
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/files/"):
			w.Write([]byte(files[strings.TrimPrefix(r.URL.Path, "/files/")].data))
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, 400)
		}
	}))
}

func TestUploadDownload(t *testing.T) {
	srv := fakeDrive(t)
	defer srv.Close()
	APIBase, UploadBase = srv.URL, srv.URL+"/upload"
	st := &memStore{t: &oauth2.Token{AccessToken: "tok", Expiry: time.Now().Add(time.Hour)}}
	s := New("client", "secret", "http://127.0.0.1/cb", st)
	ctx := context.Background()
	if b, err := s.Download(ctx, "library.json"); err != nil || b != nil {
		t.Fatalf("expected no file: %v %q", err, b)
	}
	if err := s.Upload(ctx, "library.json", "application/json", []byte(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Upload(ctx, "library.json", "application/json", []byte(`{"v":2}`)); err != nil {
		t.Fatal(err)
	}
	b, err := s.Download(ctx, "library.json")
	if err != nil || string(b) != `{"v":2}` {
		t.Fatalf("got %q %v", b, err)
	}
}

func TestAuthURL(t *testing.T) {
	s := New("client-id", "secret", "http://127.0.0.1:8080/api/drive/callback", &memStore{})
	u, err := s.AuthURL()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"drive.file", "code_challenge=", "access_type=offline", "redirect_uri=http%3A%2F%2F127.0.0.1%3A8080"} {
		if !strings.Contains(u, want) {
			t.Errorf("auth URL missing %s: %s", want, u)
		}
	}
	if New("", "", "", &memStore{}).Configured() {
		t.Error("should not be configured without a client id")
	}
}
