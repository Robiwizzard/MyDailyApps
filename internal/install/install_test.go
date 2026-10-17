package install

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/robiwizzard/mydailyapps/internal/library"
)

func TestMatchesPlatform(t *testing.T) {
	cases := []struct {
		name, goos, goarch string
		want               bool
	}{
		{"tracker_windows_amd64.zip", "windows", "amd64", true},
		{"tracker-linux-x86_64.tar.gz", "linux", "amd64", true},
		{"tracker-macos-arm64.zip", "darwin", "arm64", true},
		{"tracker-darwin-amd64.zip", "darwin", "arm64", false},
		{"tracker_windows_amd64.zip.sha256", "windows", "amd64", false},
		{"twin-linux-amd64", "windows", "amd64", false},
	}
	for _, c := range cases {
		if got := matchesPlatform(c.name, "", c.goos, c.goarch); got != c.want {
			t.Errorf("%s on %s/%s: got %v", c.name, c.goos, c.goarch, got)
		}
	}
}

func TestCommonRoot(t *testing.T) {
	if r := commonRoot([]string{"app-v1/", "app-v1/bin", "app-v1/lib/x"}); r != "app-v1/" {
		t.Errorf("got %q", r)
	}
	if r := commonRoot([]string{"bin", "lib/x"}); r != "" {
		t.Errorf("got %q", r)
	}
}

func TestReleaseInstall(t *testing.T) {
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	w, _ := zw.Create("tracker-v1/tracker")
	w.Write([]byte("#!/bin/sh\necho hi\n"))
	zw.Close()

	asset := "tracker_" + runtime.GOOS + "_" + runtime.GOARCH + ".zip"
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/tracker/releases/latest":
			json.NewEncoder(w).Encode(map[string]any{"assets": []map[string]any{
				{"name": "checksums.txt", "browser_download_url": srv.URL + "/dl/sums"},
				{"name": asset, "browser_download_url": srv.URL + "/dl/" + asset, "url": srv.URL + "/api/asset"},
			}})
		case "/dl/" + asset:
			w.Write(zbuf.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	APIBase, httpClient = srv.URL, srv.Client()

	app := library.App{ID: "tracker", Name: "Tracker", Repo: "https://github.com/owner/tracker",
		Install: []library.InstallOption{{Type: "release", Launch: "tracker"}}}
	dir := filepath.Join(t.TempDir(), "apps", "tracker")
	if Scan(app, dir).Installed {
		t.Fatal("should not be installed yet")
	}
	p, err := MakePlan(context.Background(), app, dir, Options{})
	if err != nil || p.Option != 0 {
		t.Fatalf("plan: %v %+v", err, p)
	}
	if err := Run(context.Background(), app, p, Options{}, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tracker")); err != nil {
		t.Fatal("binary not unpacked to the app folder root:", err)
	}
	if r := Scan(app, dir); !r.Installed || r.Launch != "tracker" {
		t.Fatalf("scan after install: %+v", r)
	}
}

func TestMissingRequirement(t *testing.T) {
	app := library.App{ID: "x", Name: "X", Repo: "https://github.com/o/x",
		Install: []library.InstallOption{{Type: "git", Requires: []string{"definitely-not-a-tool-xyz"}, Launch: "npm start"}}}
	p, err := MakePlan(context.Background(), app, t.TempDir()+"/x", Options{})
	if err != nil || p.Option != -1 || len(p.Missing) != 1 {
		t.Fatalf("got %v %+v", err, p)
	}
}

func TestZipSlip(t *testing.T) {
	if _, err := safeJoin("/tmp/a", "../../etc/passwd"); err == nil {
		t.Fatal("expected error")
	}
}
