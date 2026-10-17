package detect

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir string, files map[string]string) {
	for p, c := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func finding(d Draft, field string) string {
	var out []string
	for _, f := range d.Findings {
		if f.Field == field {
			out = append(out, f.Confidence+": "+f.Note)
		}
	}
	return strings.Join(out, " | ")
}

func TestNodeAppReadingEnv(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "initiative-tracker")
	write(t, dir, map[string]string{
		"package.json":        `{"scripts":{"build":"vite build","prepare":"npm run build","start":"node dist/server/index.js"},"engines":{"node":">=24"}}`,
		"package-lock.json":   `{}`,
		".gitignore":          "node_modules\ndist\ndata\n*.tmp\n",
		"src/server/index.ts": "const port = Number(process.env.PORT) || 5180;\nconst dataDir = process.env.DATA_DIR || 'data';",
		"src/client/App.tsx":  "export const App = () => null",
		"node_modules/x/a.js": "listen(9999)",
	})
	src, err := Local(dir)
	if err != nil {
		t.Fatal(err)
	}
	d := Detect(src, "https://github.com/o/initiative-tracker", []string{"D&D", "Tools"})
	if d.App.ID != "initiative-tracker" || d.App.Name != "Initiative Tracker" {
		t.Fatalf("identity: %+v", d.App)
	}
	if len(d.App.Install) != 1 || d.App.Install[0].Setup != "npm ci" || d.App.Install[0].Launch != "npm start" {
		t.Fatalf("install: %+v", d.App.Install)
	}
	if d.App.Port != 0 || !strings.Contains(finding(d, "port"), "reads the PORT") {
		t.Fatalf("port: %d %s", d.App.Port, finding(d, "port"))
	}
	if d.App.Cloud != nil || !strings.Contains(finding(d, "cloud"), "reads DATA_DIR") {
		t.Fatalf("cloud: %+v %s", d.App.Cloud, finding(d, "cloud"))
	}
	if !strings.Contains(finding(d, "install"), "Node >=24") {
		t.Fatalf("engines note missing: %s", finding(d, "install"))
	}
}

func TestAppWritingToItsOwnFolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "friends_cookbook")
	write(t, dir, map[string]string{
		"go.mod":     "module github.com/friend/cookbook\n\ngo 1.24\n",
		"main.go":    "package main\nfunc main(){ http.ListenAndServe(\":8095\", nil) }",
		".gitignore": "/saves/\n*.sqlite\nbin/\n",
	})
	src, _ := Local(dir)
	d := Detect(src, "https://github.com/friend/cookbook", []string{"Cooking"})
	if d.App.Name != "Friends Cookbook" {
		t.Fatalf("name %q", d.App.Name)
	}
	if len(d.App.Install) != 1 || d.App.Install[0].Launch != "cookbook" || d.App.Install[0].Setup != "go build -o ." {
		t.Fatalf("install: %+v", d.App.Install)
	}
	if d.App.Port != 8095 {
		t.Fatalf("fixed port not found: %d %s", d.App.Port, finding(d, "port"))
	}
	if d.App.Cloud == nil || d.App.Cloud.Root != "app" || strings.Join(d.App.Cloud.Include, ",") != "saves/**,**/*.sqlite" {
		t.Fatalf("cloud: %+v", d.App.Cloud)
	}
	if d.App.Group != "Cooking" {
		t.Fatalf("group %q", d.App.Group)
	}
}

func TestReleaseAssetsAndGitHubSource(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/tracker":
			json.NewEncoder(w).Encode(map[string]any{"name": "tracker", "description": "Combat tracker", "topics": []string{"dnd"}, "default_branch": "main"})
		case "/repos/o/tracker/git/trees/main":
			json.NewEncoder(w).Encode(map[string]any{"tree": []map[string]string{
				{"path": "go.mod", "type": "blob"}, {"path": "cmd/tracker/main.go", "type": "blob"}, {"path": "cmd", "type": "tree"}}})
		case "/repos/o/tracker/releases/latest":
			json.NewEncoder(w).Encode(map[string]any{"assets": []map[string]string{
				{"name": "tracker_v1.2.0_windows_amd64.zip"}, {"name": "tracker_v1.2.0_linux_x86_64.tar.gz"}, {"name": "checksums.txt"}}})
		case "/repos/o/tracker/contents/go.mod":
			w.Write([]byte("module github.com/o/tracker/v2\n"))
		case "/repos/o/tracker/contents/cmd/tracker/main.go":
			w.Write([]byte(`port := os.Getenv("PORT")`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	APIBase = srv.URL
	src, err := GitHub(context.Background(), "https://github.com/o/tracker", "")
	if err != nil {
		t.Fatal(err)
	}
	d := Detect(src, "https://github.com/o/tracker", []string{"D&D"})
	if len(d.App.Install) != 2 || d.App.Install[0].Type != "release" || d.App.Install[0].Launch != "tracker" {
		t.Fatalf("install: %+v", d.App.Install)
	}
	if d.App.Install[1].Setup != "go build -o . ./cmd/tracker" || d.App.Install[1].Launch != "tracker" {
		t.Fatalf("go option: %+v", d.App.Install[1])
	}
	if d.App.Group != "D&D" || d.App.Description != "Combat tracker" {
		t.Fatalf("%+v", d.App)
	}
	if !strings.Contains(finding(d, "port"), "reads the PORT") {
		t.Fatal(finding(d, "port"))
	}
}

func TestManifestHint(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "x")
	write(t, dir, map[string]string{
		"mydailyapps.json": `{"manifestVersion":1,"name":"Fancy","group":"Tools","install":[{"type":"git","launch":"run"}],"cloud":{"include":["a.db"]}}`,
	})
	src, _ := Local(dir)
	d := Detect(src, "", nil)
	if d.App.Name != "Fancy" || d.App.Cloud == nil || d.App.Install[0].Launch != "run" {
		t.Fatalf("%+v", d.App)
	}
}
