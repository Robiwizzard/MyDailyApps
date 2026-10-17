package observe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func put(t *testing.T, root, rel, content string) {
	p := filepath.Join(root, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunsAndSuggestions(t *testing.T) {
	app, data := t.TempDir(), t.TempDir()
	put(t, app, "main.js", "code") // not a git repo: everything counts, but only diffs are tracked
	b := &Book{Files: map[string]*Stat{}}

	for run := 0; run < 3; run++ {
		before := Take(app, data)
		put(t, data, "cache/thumb.png", "x")                     // same every run
		put(t, data, "logs/app.log", strings.Repeat("l", run+1)) // changes every run
		put(t, app, "saves/slot1.json", `{"hp":`+string(rune('1'+run))+`}`)
		if run == 0 {
			put(t, data, "encounters/goblins.json", `{"a":1}`)
		}
		b.AddRun(before, Take(app, data), time.Now())
	}
	// Record a save: only the encounter changes.
	before := Take(app, data)
	put(t, data, "encounters/goblins.json", `{"a":2}`)
	rec := b.MarkRecorded(Compare(before, Take(app, data)))
	if len(rec) != 1 || rec[0] != "data:encounters/goblins.json" {
		t.Fatalf("recorded %v", rec)
	}
	get := func(k string) Suggestion { return Suggest(b.Files[k], b.Runs) }
	if s := get("data:encounters/goblins.json"); !s.Keep {
		t.Fatalf("encounter should be kept: %+v", s)
	}
	if s := get("data:cache/thumb.png"); s.Keep {
		t.Fatalf("cache should be temporary: %+v", s)
	}
	if s := get("data:logs/app.log"); s.Keep {
		t.Fatalf("log should be temporary: %+v", s)
	}
	if s := get("app:saves/slot1.json"); !s.Keep {
		t.Fatalf("save in app folder should be kept: %+v", s)
	}
	if _, ok := b.Files["app:main.js"]; ok {
		t.Fatal("unchanged code file must not be tracked")
	}

	root, inc, _, err := Patterns([]*Stat{b.Files["data:encounters/goblins.json"]}, b.Files)
	if err != nil || root != "" || strings.Join(inc, ",") != "encounters/**" {
		t.Fatalf("%q %v %v", root, inc, err)
	}
	if _, _, _, err := Patterns([]*Stat{b.Files["data:encounters/goblins.json"], b.Files["app:saves/slot1.json"]}, b.Files); err == nil {
		t.Fatal("mixed roots must be refused")
	}
}

func TestSQLiteAndRecreated(t *testing.T) {
	app, data := t.TempDir(), t.TempDir()
	b := &Book{Files: map[string]*Stat{}}
	before := Take(app, data)
	put(t, data, "store.db", "SQLite format 3\x00rest")
	put(t, data, "index.bin", "same")
	b.AddRun(before, Take(app, data), time.Now())
	if !b.Files["data:store.db"].SQLite {
		t.Fatal("sqlite header not detected")
	}
	// The user sets index.bin aside; the app recreates it identically.
	s := b.Files["data:index.bin"]
	s.AsideSHA = s.SHA
	os.Remove(filepath.Join(data, "index.bin"))
	before = Take(app, data)
	put(t, data, "index.bin", "same")
	b.AddRun(before, Take(app, data), time.Now())
	if b.Files["data:index.bin"].Recreated != 1 || Suggest(b.Files["data:index.bin"], b.Runs).Keep {
		t.Fatalf("%+v", b.Files["data:index.bin"])
	}
}
