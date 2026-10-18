package host

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robiwizzard/mydailyapps/internal/config"
	"github.com/robiwizzard/mydailyapps/internal/detect"
	"github.com/robiwizzard/mydailyapps/internal/drive"
	"github.com/robiwizzard/mydailyapps/internal/drive/drivetest"
	"github.com/robiwizzard/mydailyapps/internal/library"
	"github.com/robiwizzard/mydailyapps/internal/state"
)

// fakeGitHub serves one Node repository that reads DATA_DIR.
func fakeGitHub(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/tracker":
			json.NewEncoder(w).Encode(map[string]any{"name": "tracker", "default_branch": "main"})
		case "/repos/o/tracker/git/trees/main":
			json.NewEncoder(w).Encode(map[string]any{"tree": []map[string]string{
				{"path": "package.json", "type": "blob"}, {"path": "server.js", "type": "blob"}}})
		case "/repos/o/tracker/contents/package.json":
			w.Write([]byte(`{"scripts":{"start":"node server.js"}}`))
		case "/repos/o/tracker/contents/server.js":
			w.Write([]byte(`const port = process.env.PORT; const dir = process.env.DATA_DIR;`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func newTestHost(t *testing.T) (*Host, *drivetest.Server) {
	fd, dsrv := drivetest.New()
	t.Cleanup(dsrv.Close)
	drive.APIBase, drive.UploadBase = fd.URL, fd.URL+"/upload"
	gh := fakeGitHub(t)
	t.Cleanup(gh.Close)
	detect.APIBase = gh.URL

	root := t.TempDir()
	cfg := &config.Config{Mode: config.ModePersistent, DataDir: filepath.Join(root, "data"), AppsDir: filepath.Join(root, "apps"),
		ExeDir: root, Port: 1, GoogleClientID: "client", GoogleClientSecret: "secret"}
	os.MkdirAll(cfg.DataDir, 0o700)
	os.MkdirAll(cfg.AppsDir, 0o755)
	st, err := state.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tok, _ := json.Marshal(map[string]any{"access_token": "tok", "token_type": "Bearer", "expiry": time.Now().Add(time.Hour)})
	st.SetSetting("drive_token", string(tok))
	h, err := New(cfg, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	return h, fd
}

func hasFinding(d detect.Draft, text string) bool {
	for _, f := range d.Findings {
		if strings.Contains(f.Note, text) {
			return true
		}
	}
	return false
}

func TestRemoveThenAddFindsDriveData(t *testing.T) {
	h, _ := newTestHost(t)
	ctx := context.Background()
	repo := "https://github.com/o/tracker"

	d, err := h.Detect(ctx, repo, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if d.App.ID != "tracker" || d.App.Cloud != nil {
		t.Fatalf("first draft: %+v", d.App)
	}
	d.App.Group = "D&D"
	d.App.Cloud = &library.Cloud{Include: []string{"encounters/*.json"}}
	if err := h.SaveEntry(ctx, "", d.App); err != nil {
		t.Fatal(err)
	}
	h.WaitSyncs(10 * time.Second)
	// The app ran and its data was synced.
	dataDir := h.Cfg.AppDataDir("tracker")
	os.MkdirAll(filepath.Join(dataDir, "encounters"), 0o700)
	os.WriteFile(filepath.Join(dataDir, "encounters", "goblins.json"), []byte(`{}`), 0o600)
	if rep, err := h.Sync.Push(ctx, "tracker", dataDir, d.App.Cloud); err != nil || len(rep.Uploaded) != 1 {
		t.Fatalf("push: %+v %v", rep, err)
	}

	if err := h.RemoveEntry(ctx, "tracker"); err != nil {
		t.Fatal(err)
	}
	again, err := h.Detect(ctx, repo, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if again.App.Cloud == nil || strings.Join(again.App.Cloud.Include, ",") != "encounters/*.json" || again.App.Group != "D&D" {
		t.Fatalf("earlier settings not restored: %+v %+v", again.App, again.App.Cloud)
	}
	if !hasFinding(again, "was in your library before") || !hasFinding(again, "1 synced file") {
		t.Fatalf("findings: %+v", again.Findings)
	}
}

func TestOlderDriveFolderWithoutSettingsCopy(t *testing.T) {
	h, _ := newTestHost(t)
	ctx := context.Background()
	// Files synced by an older version: no settings copy in the folder.
	dataDir := t.TempDir()
	os.MkdirAll(filepath.Join(dataDir, "saves"), 0o700)
	os.WriteFile(filepath.Join(dataDir, "saves", "slot.json"), []byte(`1`), 0o600)
	os.WriteFile(filepath.Join(dataDir, "notes.db"), []byte(`2`), 0o600)
	if _, err := h.Sync.Push(ctx, "tracker", dataDir, &library.Cloud{Include: []string{"**"}}); err != nil {
		t.Fatal(err)
	}
	d, err := h.Detect(ctx, "https://github.com/o/tracker", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if d.App.Cloud == nil || strings.Join(d.App.Cloud.Include, ",") != "notes.db,saves/**" {
		t.Fatalf("cloud not rebuilt from the Drive files: %+v", d.App.Cloud)
	}
}

func TestDeleteEverything(t *testing.T) {
	h, fd := newTestHost(t)
	ctx := context.Background()
	e := library.App{ID: "tracker", Name: "Tracker", Repo: "https://github.com/o/tracker",
		Install: []library.InstallOption{{Type: "git", Launch: "npm start"}}, Cloud: &library.Cloud{Include: []string{"*.json"}}}
	if err := h.SaveEntry(ctx, "", e); err != nil {
		t.Fatal(err)
	}
	h.WaitSyncs(10 * time.Second)
	appDir := filepath.Join(h.Cfg.AppsDir, "tracker")
	os.MkdirAll(filepath.Join(appDir, ".git", "objects"), 0o755)
	ro := filepath.Join(appDir, ".git", "objects", "pack")
	os.WriteFile(ro, []byte("x"), 0o444) // git keeps objects read-only
	dataDir := h.Cfg.AppDataDir("tracker")
	os.MkdirAll(dataDir, 0o700)
	os.WriteFile(filepath.Join(dataDir, "a.json"), []byte(`{}`), 0o600)
	h.Sync.Push(ctx, "tracker", dataDir, e.Cloud)
	h.Store.SetApproved("tracker", "x")

	report, err := h.DeleteApp(ctx, "tracker")
	if err != nil {
		t.Fatal(err)
	}
	t.Log(strings.Join(report, "\n"))
	for _, p := range []string{appDir, dataDir} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s still exists", p)
		}
	}
	if l, _ := h.Lib.Get(); len(l.Apps) != 0 {
		t.Fatal("still in the library")
	}
	if s, _ := h.Store.App("tracker"); s.Approved != "" {
		t.Fatal("local state not forgotten")
	}
	for _, f := range fd.Live() {
		if f.Name == "tracker" || strings.Contains(f.Name, "a.json") || f.Name == drive.AppMetaName {
			t.Fatalf("Drive still has %s", f.Name)
		}
	}
	// Adding it again starts fresh: nothing to restore.
	d, err := h.Detect(ctx, "https://github.com/o/tracker", "", "")
	if err != nil || d.App.Cloud != nil || hasFinding(d, "was in your library before") {
		t.Fatalf("%+v %v", d, err)
	}
}

func TestDeleteRefusesFoldersOutsideApps(t *testing.T) {
	h, _ := newTestHost(t)
	for _, dir := range []string{h.Cfg.AppsDir, h.Cfg.DataDir, h.Cfg.ExeDir, filepath.Dir(h.Cfg.AppsDir)} {
		if err := h.safeToDelete(dir, true); err == nil {
			t.Errorf("%s should be refused", dir)
		}
	}
	if err := h.safeToDelete(filepath.Join(h.Cfg.AppsDir, "x"), false); err != nil {
		t.Error(err)
	}
	if err := h.safeToDelete(t.TempDir(), false); err == nil {
		t.Error("a folder outside apps/ without a custom path should be refused")
	}
}

func driveHas(fd *drivetest.Server, name string) bool {
	for _, f := range fd.Live() {
		if f.Name == name {
			return true
		}
	}
	return false
}

func TestNewSavePathsSyncRightAway(t *testing.T) {
	h, fd := newTestHost(t)
	ctx := context.Background()
	e := library.App{ID: "tracker", Name: "Tracker", Repo: "https://github.com/o/tracker",
		Install: []library.InstallOption{{Type: "git", Launch: "npm start"}}}
	if err := h.SaveEntry(ctx, "", e); err != nil {
		t.Fatal(err)
	}
	dataDir := h.Cfg.AppDataDir("tracker")
	os.MkdirAll(filepath.Join(dataDir, "saves"), 0o700)
	os.WriteFile(filepath.Join(dataDir, "saves", "slot1.json"), []byte(`1`), 0o600)
	os.WriteFile(filepath.Join(dataDir, "saves", "slot2.json"), []byte(`2`), 0o600)

	// Ticking the save folder in Files (or Edit) uploads it without a start.
	e.Cloud = &library.Cloud{Include: []string{"saves/**"}}
	if err := h.SaveEntry(ctx, "tracker", e); err != nil {
		t.Fatal(err)
	}
	h.WaitSyncs(10 * time.Second)
	if !driveHas(fd, "saves/slot1.json") || !driveHas(fd, "saves/slot2.json") {
		t.Fatalf("not uploaded: %+v", fd.Live())
	}
	if v := h.Apps(); len(v) != 1 || !strings.Contains(v[0].SyncNote, "2 uploaded") {
		t.Fatalf("card note: %+v", v)
	}

	// Saving the same settings again does not sync.
	h.mu.Lock()
	delete(h.syncNote, "tracker")
	h.mu.Unlock()
	h.SaveEntry(ctx, "tracker", e)
	h.WaitSyncs(10 * time.Second)
	if v := h.Apps(); v[0].SyncNote != "" {
		t.Fatalf("unexpected sync: %s", v[0].SyncNote)
	}

	// A deleted save is trashed in Drive by the next sync, not uploaded back.
	os.Remove(filepath.Join(dataDir, "saves", "slot2.json"))
	if err := h.SyncNow(ctx, "tracker"); err != nil {
		t.Fatal(err)
	}
	if driveHas(fd, "saves/slot2.json") || !driveHas(fd, "saves/slot1.json") {
		t.Fatalf("slot2 should be in the Drive trash: %+v", fd.Live())
	}
	if _, err := os.Stat(filepath.Join(dataDir, "saves", "slot2.json")); !os.IsNotExist(err) {
		t.Fatal("slot2 came back")
	}
}
