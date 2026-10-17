package install

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/robiwizzard/mydailyapps/internal/library"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestGitUpdate(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	work := filepath.Join(root, "work")
	app := filepath.Join(root, "apps", "x")
	exec.Command("git", "init", "-q", "--bare", origin).Run()
	exec.Command("git", "clone", "-q", origin, work).Run()
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("v1"), 0o644)
	git(t, work, "add", ".")
	git(t, work, "commit", "-qm", "v1")
	git(t, work, "push", "-q", "origin", "HEAD")
	os.MkdirAll(filepath.Dir(app), 0o755)
	if err := gitClone(context.Background(), origin, app, "", func(string) {}); err != nil {
		t.Fatal(err)
	}
	a := library.App{ID: "x", Repo: origin}
	ctx := context.Background()

	if st, err := CheckUpdate(ctx, a, app, ""); err != nil || st.Available {
		t.Fatalf("expected up to date: %+v %v", st, err)
	}
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("v2"), 0o644)
	git(t, work, "commit", "-qam", "v2")
	git(t, work, "push", "-q", "origin", "HEAD")
	st, err := CheckUpdate(ctx, a, app, "")
	if err != nil || !st.Available || st.Detail != "1 new commit" {
		t.Fatalf("expected an update: %+v %v", st, err)
	}

	// Local edits block the update instead of being overwritten.
	os.WriteFile(filepath.Join(app, "a.txt"), []byte("edited"), 0o644)
	if err := Update(ctx, a, app, "", func() string { return "" }, func(string) {}); err != ErrLocalChanges {
		t.Fatalf("expected ErrLocalChanges, got %v", err)
	}
	git(t, app, "checkout", "--", "a.txt")

	setupRan := false
	err = Update(ctx, a, app, "", func() string { setupRan = true; return "echo setup" }, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(app, "a.txt")); string(b) != "v2" || !setupRan {
		t.Fatalf("not updated: %q setup=%v", b, setupRan)
	}
	if st, _ := CheckUpdate(ctx, a, app, ""); st.Available {
		t.Fatal("still shows an update after updating")
	}
}

func TestReleaseUpdate(t *testing.T) {
	mkzip := func(content string) []byte {
		var b bytes.Buffer
		zw := zip.NewWriter(&b)
		w, _ := zw.Create("tracker")
		w.Write([]byte(content))
		zw.Close()
		return b.Bytes()
	}
	asset := "tracker_" + runtime.GOOS + "_" + runtime.GOARCH + ".zip"
	tag, data := "v1", mkzip("one")
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/tracker/releases/latest":
			json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "assets": []map[string]any{
				{"name": asset, "browser_download_url": srv.URL + "/dl"}}})
		case "/dl":
			w.Write(data)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	APIBase, httpClient = srv.URL, srv.Client()
	ctx := context.Background()
	app := library.App{ID: "tracker", Repo: "https://github.com/o/tracker", Install: []library.InstallOption{{Type: "release", Launch: "tracker"}}}
	dir := filepath.Join(t.TempDir(), "tracker")
	p, err := MakePlan(ctx, app, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, app, p, Options{}, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if st, _ := CheckUpdate(ctx, app, dir, ""); st.Available {
		t.Fatal("no update expected yet")
	}
	tag, data = "v2", mkzip("two")
	st, err := CheckUpdate(ctx, app, dir, "")
	if err != nil || !st.Available || st.Detail != "v1 → v2" {
		t.Fatalf("%+v %v", st, err)
	}
	if err := Update(ctx, app, dir, "", func() string { return "" }, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "tracker")); string(b) != "two" {
		t.Fatalf("got %q", b)
	}
	if m, _ := ReadMarker(dir); m.ReleaseTag != "v2" {
		t.Fatalf("marker tag %q", m.ReleaseTag)
	}
	if _, err := os.Stat(dir + ".old"); !os.IsNotExist(err) {
		t.Fatal("old folder left behind")
	}
}
