// Package drivetest is a small in-memory fake of the Google Drive API calls
// MyDailyApps uses, for tests.
package drivetest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"time"
)

const folderMime = "application/vnd.google-apps.folder"

// New starts a fake Drive server. Point drive.APIBase at URL and
// drive.UploadBase at URL+"/upload".
func New() (*Server, *httptest.Server) {
	f := &Server{Files: map[string]*File{}, sessions: map[string]*File{}}
	srv := httptest.NewServer(f)
	f.URL = srv.URL
	return f, srv
}

// trash marks a file and everything under it (folders) as trashed.
func (f *Server) trash(id string, v bool) {
	if x := f.Files[id]; x != nil {
		x.Trashed = v
	}
	for _, x := range f.Files {
		if x.Parent == id {
			f.trash(x.ID, v)
		}
	}
}

// Live returns the files that are not trashed.
func (f *Server) Live() []*File {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*File
	for _, x := range f.Files {
		if !x.Trashed {
			out = append(out, x)
		}
	}
	return out
}

type File struct {
	ID, Name, Parent, Mime, Data string
	Trashed                      bool
	Props                        map[string]string
	Modified                     time.Time
}

// Server implements the Drive calls used for app sync, including
// resumable uploads and server-side copies.
type Server struct {
	mu       sync.Mutex
	Files    map[string]*File
	sessions map[string]*File // pending upload sessions
	n        int
	URL      string
}

var (
	reName   = regexp.MustCompile(`name = '([^']*)'`)
	reParent = regexp.MustCompile(`'([^']*)' in parents`)
)

func (f *Server) id() string { f.n++; return fmt.Sprint("f", f.n) }

func (f *Server) meta(x *File) map[string]any {
	return map[string]any{"id": x.ID, "name": x.Name, "size": fmt.Sprint(len(x.Data)),
		"modifiedTime": x.Modified.Format(time.RFC3339Nano), "appProperties": x.Props}
}

func (f *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
		for _, x := range f.Files {
			if x.Trashed {
				continue
			}
			if parent != "" && parent != "root" && x.Parent != parent {
				continue
			}
			if parent == "root" && x.Parent != "" {
				continue
			}
			if name != "" && x.Name != name {
				continue
			}
			if strings.Contains(q, "mimeType = 'application/vnd.google-apps.folder'") && x.Mime != folderMime {
				continue
			}
			if strings.Contains(q, "mimeType != 'application/vnd.google-apps.folder'") && x.Mime == folderMime {
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
		x := &File{ID: f.id(), Name: m.Name, Mime: m.MimeType, Modified: time.Now()}
		if len(m.Parents) > 0 {
			x.Parent = m.Parents[0]
		}
		f.Files[x.ID] = x
		json.NewEncoder(w).Encode(map[string]string{"id": x.ID})
	case r.Method == "PATCH" && strings.HasPrefix(p, "/upload/files/") && r.URL.Query().Get("uploadType") == "media":
		x := f.Files[strings.TrimPrefix(p, "/upload/files/")]
		b, _ := io.ReadAll(r.Body)
		x.Data = string(b)
		json.NewEncoder(w).Encode(f.meta(x))
	case r.Method == "PATCH" && strings.HasPrefix(p, "/files/"):
		var m struct {
			Trashed bool
		}
		json.NewDecoder(r.Body).Decode(&m)
		id := strings.TrimPrefix(p, "/files/")
		f.trash(id, m.Trashed)
		w.Write([]byte("{}"))
	case (r.Method == "POST" && p == "/upload/files") || (r.Method == "PATCH" && strings.HasPrefix(p, "/upload/files/")):
		var m struct {
			Name          string
			Parents       []string
			AppProperties map[string]string
		}
		json.NewDecoder(r.Body).Decode(&m)
		var x *File
		if r.Method == "PATCH" {
			old := f.Files[strings.TrimPrefix(p, "/upload/files/")]
			cp := *old
			x = &cp
		} else {
			x = &File{ID: f.id(), Name: m.Name}
			if len(m.Parents) > 0 {
				x.Parent = m.Parents[0]
			}
		}
		if m.AppProperties != nil {
			x.Props = m.AppProperties
		}
		sid := f.id()
		f.sessions[sid] = x
		w.Header().Set("Location", f.URL+"/session/"+sid)
	case r.Method == "PUT" && strings.HasPrefix(p, "/session/"):
		x := f.sessions[strings.TrimPrefix(p, "/session/")]
		b, _ := io.ReadAll(r.Body)
		if r.ContentLength != int64(len(b)) {
			http.Error(w, "length", 400)
			return
		}
		x.Data, x.Modified = string(b), time.Now()
		f.Files[x.ID] = x
		json.NewEncoder(w).Encode(f.meta(x))
	case r.Method == "GET" && strings.HasPrefix(p, "/files/"):
		x := f.Files[strings.TrimPrefix(p, "/files/")]
		io.WriteString(w, x.Data)
	case r.Method == "POST" && strings.HasSuffix(p, "/copy"):
		src := f.Files[strings.TrimSuffix(strings.TrimPrefix(p, "/files/"), "/copy")]
		var m struct {
			Name    string
			Parents []string
		}
		json.NewDecoder(r.Body).Decode(&m)
		x := &File{ID: f.id(), Name: m.Name, Parent: m.Parents[0], Data: src.Data, Modified: time.Now()}
		f.Files[x.ID] = x
		json.NewEncoder(w).Encode(map[string]string{"id": x.ID})
	default:
		http.Error(w, "unexpected "+r.Method+" "+p, 400)
	}
}
