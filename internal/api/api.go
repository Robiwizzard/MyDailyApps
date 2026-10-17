// Package api is the HTTP layer: JSON endpoints under /api and the embedded UI.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/robiwizzard/mydailyapps/internal/host"
	"github.com/robiwizzard/mydailyapps/internal/install"
	"github.com/robiwizzard/mydailyapps/internal/library"
)

// CSRFHeader must be sent with every state-changing request. Other websites
// cannot add custom headers to cross-origin requests without a CORS
// preflight, which this server never approves.
const CSRFHeader = "X-MyDailyApps"

// Handler builds the router.
func Handler(h *host.Host, ui fs.FS, port int) http.Handler {
	s := &server{h: h, port: port}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/ping", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]any{"ok": true}) })
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/apps", s.apps)
	mux.HandleFunc("POST /api/rescan", s.rescan)
	mux.HandleFunc("POST /api/apps/{id}/install", s.install)
	mux.HandleFunc("POST /api/apps/{id}/launch", s.launch)
	mux.HandleFunc("POST /api/apps/{id}/stop", s.stop(false))
	mux.HandleFunc("POST /api/apps/{id}/force-stop", s.stop(true))
	mux.HandleFunc("GET /api/apps/{id}/logs", s.logs)
	mux.HandleFunc("POST /api/apps/{id}/sync", s.sync)
	mux.HandleFunc("POST /api/apps/{id}/update", s.update)
	mux.HandleFunc("POST /api/updates/check", s.checkUpdates)
	mux.HandleFunc("POST /api/detect", s.detect)
	mux.HandleFunc("POST /api/apps", s.addEntry)
	mux.HandleFunc("GET /api/apps/{id}/config", s.getEntry)
	mux.HandleFunc("PUT /api/apps/{id}/config", s.putEntry)
	mux.HandleFunc("DELETE /api/apps/{id}", s.removeEntry)
	mux.HandleFunc("GET /api/apps/{id}/files", s.files)
	mux.HandleFunc("POST /api/apps/{id}/files/record", s.record)
	mux.HandleFunc("POST /api/apps/{id}/files/aside", s.aside)
	mux.HandleFunc("POST /api/apps/{id}/files/apply", s.applyFiles)
	mux.HandleFunc("GET /api/library", s.getLibrary)
	mux.HandleFunc("PUT /api/library", s.putLibrary)
	mux.HandleFunc("GET /api/settings", s.getSettings)
	mux.HandleFunc("PUT /api/settings", s.putSettings)
	mux.HandleFunc("POST /api/drive/connect", s.driveConnect)
	mux.HandleFunc("GET /api/drive/callback", s.driveCallback)
	mux.HandleFunc("POST /api/drive/disconnect", s.driveDisconnect)
	mux.HandleFunc("POST /api/drive/sync", s.driveSync)
	mux.HandleFunc("POST /api/shutdown", s.shutdown)
	mux.Handle("/", spa(ui))
	return s.guard(mux)
}

type server struct {
	h    *host.Host
	port int
	mu   sync.Mutex
}

// guard blocks DNS-rebinding (Host must be localhost) and cross-site requests.
func (s *server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hostname := r.Host
		if hn, _, err := net.SplitHostPort(r.Host); err == nil {
			hostname = hn
		}
		switch strings.Trim(hostname, "[]") {
		case "localhost", "127.0.0.1", "::1":
		default:
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get(CSRFHeader) != "1" {
			writeErr(w, http.StatusForbidden, errors.New("missing "+CSRFHeader+" header"))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.h.Status())
}

func (s *server) apps(w http.ResponseWriter, r *http.Request) {
	st := s.h.Status()
	writeJSON(w, 200, map[string]any{"apps": s.h.Apps(), "groups": s.h.Groups(), "version": st.Version, "ready": st.Ready})
}

func (s *server) rescan(w http.ResponseWriter, r *http.Request) {
	s.h.Rescan()
	s.apps(w, r)
}

func (s *server) findApp(id string) (host.AppView, bool) {
	for _, a := range s.h.Apps() {
		if a.ID == id {
			return a, true
		}
	}
	return host.AppView{}, false
}

func (s *server) install(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Confirm bool   `json:"confirm"`
		Token   string `json:"token"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	var logMu sync.Mutex
	var lines []string
	err := s.h.Install(r.Context(), id, body.Confirm, body.Token, func(l string) {
		logMu.Lock()
		lines = append(lines, l)
		logMu.Unlock()
	})
	if s.handleCommonErr(w, err, lines) {
		return
	}
	a, _ := s.findApp(id)
	writeJSON(w, 200, map[string]any{"app": a, "log": lines})
}

func (s *server) launch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Confirm bool `json:"confirm"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	url, err := s.h.Launch(r.Context(), id, body.Confirm)
	if s.handleCommonErr(w, err, nil) {
		return
	}
	writeJSON(w, 200, map[string]any{"url": url})
}

// handleCommonErr writes typed errors. Returns true if it wrote a response.
func (s *server) handleCommonErr(w http.ResponseWriter, err error, lines []string) bool {
	if err == nil {
		return false
	}
	var ce *host.ConfirmError
	var te *host.TokenError
	var me *host.MissingError
	switch {
	case errors.As(err, &ce):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "confirm": ce})
	case errors.As(err, &te):
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": te.Message, "needsToken": true})
	case errors.As(err, &me):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": me.Error(), "missing": me.Missing, "problems": me.Problems})
	case errors.Is(err, host.ErrNotFound):
		writeErr(w, 404, err)
	case errors.Is(err, install.ErrNeedsToken):
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": err.Error(), "needsToken": true})
	default:
		writeJSON(w, 500, map[string]any{"error": err.Error(), "log": tail(lines, 30)})
	}
	return true
}

func (s *server) stop(force bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := s.h.Stop(id, force); err != nil {
			s.handleCommonErr(w, err, nil)
			return
		}
		a, _ := s.findApp(id)
		writeJSON(w, 200, map[string]any{"app": a})
	}
}

func (s *server) update(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Confirm bool   `json:"confirm"`
		Token   string `json:"token"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	var logMu sync.Mutex
	var lines []string
	err := s.h.Update(r.Context(), id, body.Confirm, body.Token, func(l string) {
		logMu.Lock()
		lines = append(lines, l)
		logMu.Unlock()
	})
	if s.handleCommonErr(w, err, lines) {
		return
	}
	a, _ := s.findApp(id)
	writeJSON(w, 200, map[string]any{"app": a, "log": lines})
}

func (s *server) detect(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Repo  string `json:"repo"`
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	d, err := s.h.Detect(r.Context(), body.Repo, body.ID, body.Token)
	if s.handleCommonErr(w, err, nil) {
		return
	}
	writeJSON(w, 200, d)
}

func (s *server) addEntry(w http.ResponseWriter, r *http.Request) {
	var e library.App
	if err := readJSON(r, &e); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.h.SaveEntry(r.Context(), "", e); err != nil {
		writeErr(w, 400, err)
		return
	}
	s.apps(w, r)
}

func (s *server) getEntry(w http.ResponseWriter, r *http.Request) {
	e, err := s.h.Entry(r.PathValue("id"))
	if s.handleCommonErr(w, err, nil) {
		return
	}
	writeJSON(w, 200, e)
}

func (s *server) putEntry(w http.ResponseWriter, r *http.Request) {
	var e library.App
	if err := readJSON(r, &e); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.h.SaveEntry(r.Context(), r.PathValue("id"), e); err != nil {
		if errors.Is(err, host.ErrNotFound) {
			writeErr(w, 404, err)
		} else {
			writeErr(w, 400, err)
		}
		return
	}
	s.apps(w, r)
}

func (s *server) removeEntry(w http.ResponseWriter, r *http.Request) {
	if err := s.h.RemoveEntry(r.Context(), r.PathValue("id")); err != nil {
		if errors.Is(err, host.ErrNotFound) {
			writeErr(w, 404, err)
		} else {
			writeErr(w, 409, err)
		}
		return
	}
	s.apps(w, r)
}

func (s *server) files(w http.ResponseWriter, r *http.Request) {
	info, err := s.h.Files(r.PathValue("id"))
	if s.handleCommonErr(w, err, nil) {
		return
	}
	writeJSON(w, 200, info)
}

func (s *server) record(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Start bool `json:"start"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	keys, err := s.h.Record(r.PathValue("id"), body.Start)
	if err != nil {
		writeErr(w, 409, err)
		return
	}
	info, _ := s.h.Files(r.PathValue("id"))
	writeJSON(w, 200, map[string]any{"changed": keys, "files": info})
}

func (s *server) aside(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key    string `json:"key"`
		Action string `json:"action"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.h.Aside(r.PathValue("id"), body.Key, body.Action); err != nil {
		writeErr(w, 409, err)
		return
	}
	s.files(w, r)
}

func (s *server) applyFiles(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Keys []string `json:"keys"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	if _, err := s.h.ApplyFiles(r.Context(), r.PathValue("id"), body.Keys); err != nil {
		writeErr(w, 400, err)
		return
	}
	s.files(w, r)
}

func (s *server) checkUpdates(w http.ResponseWriter, r *http.Request) {
	s.h.CheckUpdates(r.Context())
	s.apps(w, r)
}

func (s *server) sync(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.h.SyncNow(r.Context(), id); err != nil {
		if errors.Is(err, host.ErrNotFound) {
			writeErr(w, 404, err)
		} else {
			writeErr(w, 409, err)
		}
		return
	}
	a, _ := s.findApp(id)
	writeJSON(w, 200, map[string]any{"app": a})
}

func (s *server) logs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"lines": s.h.Procs.Logs(r.PathValue("id"))})
}

func (s *server) getLibrary(w http.ResponseWriter, r *http.Request) {
	l, _ := s.h.Lib.Get()
	b, _ := l.Marshal()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="library.json"`)
	w.Write(b)
}

func (s *server) putLibrary(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 5<<20))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.h.ImportLibrary(r.Context(), b); err != nil {
		writeErr(w, 400, err)
		return
	}
	s.apps(w, r)
}

func (s *server) getSettings(w http.ResponseWriter, r *http.Request) {
	st, err := s.h.Settings()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, st)
}

func (s *server) putSettings(w http.ResponseWriter, r *http.Request) {
	var u host.SettingsUpdate
	if err := readJSON(r, &u); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.h.UpdateSettings(r.Context(), u); err != nil {
		writeErr(w, 400, err)
		return
	}
	s.getSettings(w, r)
}

func (s *server) driveConnect(w http.ResponseWriter, r *http.Request) {
	u, err := s.h.Drive.AuthURL()
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]any{"authUrl": u})
}

func (s *server) driveCallback(w http.ResponseWriter, r *http.Request) {
	err := s.h.Drive.HandleCallback(r.Context(), r.URL.Query())
	msg := "Signed in to Google Drive. You can close this tab and return to MyDailyApps."
	if err == nil {
		_ = s.h.Drive.Hello(r.Context())
		if err2 := s.h.SyncLibraryFromDrive(r.Context()); err2 != nil {
			msg = "Signed in, but the library could not be loaded from Drive: " + err2.Error()
		} else {
			s.h.Rescan()
		}
	} else {
		msg = err.Error()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, simplePage, "Google Drive", html.EscapeString(msg))
}

func (s *server) driveDisconnect(w http.ResponseWriter, r *http.Request) {
	if err := s.h.Drive.Disconnect(r.Context()); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.getSettings(w, r)
}

func (s *server) driveSync(w http.ResponseWriter, r *http.Request) {
	if err := s.h.SyncLibraryFromDrive(r.Context()); err != nil {
		writeErr(w, 502, err)
		return
	}
	s.h.Rescan()
	s.apps(w, r)
}

func (s *server) shutdown(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"ok": true})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		s.h.RequestShutdown()
	}()
}

const simplePage = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>%s</title>
<style>body{margin:0;min-height:100vh;display:grid;place-items:center;background:#05070d;color:#e6ecff;font:16px system-ui,sans-serif;padding:16px;box-sizing:border-box}p{max-width:520px;text-align:center;line-height:1.5}</style></head>
<body><p>%s</p></body></html>`

func spa(ui fs.FS) http.Handler {
	files := http.FileServer(http.FS(ui))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if strings.HasPrefix(p, "api/") {
			writeErr(w, 404, errors.New("not found"))
			return
		}
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(ui, p); err != nil {
			p = "index.html"
			r.URL.Path = "/"
		}
		if _, err := fs.Stat(ui, "index.html"); err != nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, simplePage, "MyDailyApps", "The UI was not built into this executable. Run the build script (it builds ui/ first, then the Go binary).")
			return
		}
		if p == "index.html" {
			w.Header().Set("Cache-Control", "no-cache")
		} else if strings.HasPrefix(p, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		files.ServeHTTP(w, r)
	})
}

func readJSON(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"error": err.Error()})
}

func tail(l []string, n int) []string {
	if len(l) > n {
		return l[len(l)-n:]
	}
	return l
}
