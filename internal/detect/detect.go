package detect

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/robiwizzard/mydailyapps/internal/library"
)

// Finding explains one drafted field.
type Finding struct {
	Field      string `json:"field"`      // name, group, install, port, cloud, ...
	Confidence string `json:"confidence"` // detected, guessed, missing
	Note       string `json:"note"`
}

// Draft is a proposed library entry plus the reasons behind it.
type Draft struct {
	App      library.App `json:"app"`
	Findings []Finding   `json:"findings"`
}

func (d *Draft) note(field, conf, format string, a ...any) {
	d.Findings = append(d.Findings, Finding{Field: field, Confidence: conf, Note: fmt.Sprintf(format, a...)})
}

// Detect drafts an entry for repo from what src contains. groups are the
// library's existing group names (a match is preferred over a new one).
func Detect(src Source, repo string, groups []string) Draft {
	var d Draft
	info := src.Info()
	d.App.Repo = repo

	// A manifest the author ships is the best hint, but every field can still be edited.
	var hint *library.App
	if has(src, library.ManifestName) {
		if b, err := src.Read(library.ManifestName); err == nil {
			if m, err := library.ParseManifest(b); err == nil {
				hint = m
				d.note("all", "detected", "The repository has its own %s; its values were used where present.", library.ManifestName)
			} else {
				d.note("all", "guessed", "The repository's %s is invalid and was ignored: %v", library.ManifestName, err)
			}
		}
	}

	// --- identity ---
	d.App.ID = slug(info.Name)
	if hint != nil && hint.ID != "" {
		d.App.ID = hint.ID
	}
	d.App.Name = titleCase(info.Name)
	d.note("name", "guessed", "From the repository name %q.", info.Name)
	if hint != nil && hint.Name != "" {
		d.App.Name = hint.Name
	}
	d.App.Description = info.Description
	if hint != nil && hint.Description != "" {
		d.App.Description = hint.Description
	}
	d.App.Group = guessGroup(info.Topics, groups, titleCase(info.Name)+" "+info.Description)
	if hint != nil && hint.Group != "" {
		d.App.Group = hint.Group
	}
	if d.App.Group == "" {
		d.note("group", "missing", "Pick a group (none matched the repository's topics).")
	} else {
		d.note("group", "guessed", "Matched from the repository's topics or description.")
	}

	// --- install options: a release first (works without Go/Node), then git ---
	if opt, ok := releaseOption(src.ReleaseAssets()); ok {
		d.App.Install = append(d.App.Install, opt)
		d.note("install", "detected", "The newest release has packages for different systems (%s).", strings.Join(firstN(src.ReleaseAssets(), 3), ", "))
	}
	if opt, notes, ok := gitOption(src); ok {
		d.App.Install = append(d.App.Install, opt)
		for _, n := range notes {
			d.note("install", n.Confidence, "%s", n.Note)
		}
	}
	if hint != nil && len(hint.Install) > 0 {
		d.App.Install = hint.Install
	}
	if len(d.App.Install) == 0 {
		d.note("install", "missing", "No release packages and no known project type (package.json, go.mod, pyproject.toml, requirements.txt, Cargo.toml). Add the install and start commands by hand.")
	}

	// --- port and data folder, from the source code ---
	scan := scanSource(src)
	switch {
	case scan.readsPort:
		d.note("port", "detected", "The code reads the PORT variable (%s), so MyDailyApps can pick a free port.", scan.portWhere)
	case scan.fixedPort > 0:
		d.App.Port = scan.fixedPort
		d.note("port", "guessed", "The code does not seem to read PORT; it looks like it always listens on %d (%s).", scan.fixedPort, scan.portWhere)
	default:
		d.note("port", "guessed", "Could not tell which port the app uses. If it does not read PORT, set a fixed port.")
	}
	if hint != nil && hint.Port != 0 {
		d.App.Port = hint.Port
	}
	if hint != nil && hint.Path != "" {
		d.App.Path = hint.Path
	}

	dataPatterns := gitignoreData(src)
	switch {
	case hint != nil && hint.Cloud != nil:
		d.App.Cloud = hint.Cloud
	case scan.readsDataDir:
		d.note("cloud", "guessed", "The code reads DATA_DIR (%s). Run the app once, then use Files on its card to choose what to keep in Drive.", scan.dataWhere)
	case len(dataPatterns) > 0:
		d.App.Cloud = &library.Cloud{Root: "app", Include: dataPatterns}
		d.note("cloud", "guessed", "The app does not seem to read DATA_DIR, but its .gitignore excludes data-like paths (%s), so those are synced from the app's own folder. Check with Files after the first run.", strings.Join(dataPatterns, ", "))
	default:
		d.note("cloud", "missing", "Nothing to sync was found. Run the app once and use Files on its card to see what it writes.")
	}
	return d
}

// --- identity helpers ---------------------------------------------------------

var nonID = regexp.MustCompile(`[^a-z0-9._-]+`)

func slug(s string) string {
	s = nonID.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-")
	s = strings.Trim(s, "-._")
	if len(s) > 64 {
		s = s[:64]
	}
	if s == "" {
		s = "app"
	}
	return s
}

func titleCase(s string) string {
	words := strings.FieldsFunc(s, func(r rune) bool { return r == '-' || r == '_' || r == ' ' || r == '.' })
	for i, w := range words {
		if w == strings.ToLower(w) && len(w) > 0 {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

var groupAliases = map[string][]string{
	"D&D":     {"dnd", "d-and-d", "dungeons-and-dragons", "ttrpg", "rpg", "tabletop", "pathfinder"},
	"Cooking": {"cooking", "cookbook", "recipes", "recipe", "food", "meal-planner", "kitchen"},
	"Tools":   {"tool", "tools", "utility", "utilities", "cli"},
	"Games":   {"game", "games", "gaming"},
}

func guessGroup(topics, groups []string, text string) string {
	lower := strings.ToLower(text)
	for _, g := range groups { // an existing group named in the topics or text
		lg := strings.ToLower(g)
		for _, t := range topics {
			if strings.ToLower(t) == lg {
				return g
			}
		}
	}
	for g, aliases := range groupAliases {
		for _, a := range aliases {
			for _, t := range topics {
				if strings.ToLower(t) == a {
					return existingOr(groups, g)
				}
			}
		}
	}
	for g, aliases := range groupAliases {
		for _, a := range aliases {
			if len(a) > 3 && regexp.MustCompile(`\b`+regexp.QuoteMeta(a)+`\b`).MatchString(lower) {
				return existingOr(groups, g)
			}
		}
	}
	return ""
}

func existingOr(groups []string, g string) string {
	for _, x := range groups {
		if strings.EqualFold(x, g) {
			return x
		}
	}
	return g
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return append(append([]string{}, s[:n]...), "…")
	}
	return s
}

// --- release assets -----------------------------------------------------------

var osTokens = []string{"windows", "win64", "win32", "win", "darwin", "macos", "mac", "osx", "linux"}
var archTokens = []string{"amd64", "x86_64", "x64", "arm64", "aarch64", "386", "i386"}
var versionToken = regexp.MustCompile(`^v?\d+(\.\d+)*$`)

func tokens(name string) []string {
	n := strings.ReplaceAll(strings.ToLower(name), "x86_64", "amd64")
	return strings.FieldsFunc(n, func(r rune) bool { return r == '-' || r == '_' || r == '.' || r == ' ' })
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func releaseOption(assets []string) (library.InstallOption, bool) {
	count := 0
	launch := ""
	for _, a := range assets {
		t := tokens(a)
		hasOS, hasArch := false, false
		for _, x := range t {
			hasOS = hasOS || contains(osTokens, x)
			hasArch = hasArch || contains(archTokens, x)
		}
		if !hasOS || !hasArch {
			continue
		}
		count++
		if launch == "" {
			var name []string
			for _, x := range t {
				if contains(osTokens, x) || contains(archTokens, x) || versionToken.MatchString(x) {
					break
				}
				name = append(name, x)
			}
			launch = strings.Join(name, "-")
		}
	}
	if count == 0 || launch == "" {
		return library.InstallOption{}, false
	}
	return library.InstallOption{Type: "release", Launch: launch}, true
}

// --- git option by project type -----------------------------------------------

func gitOption(src Source) (library.InstallOption, []Finding, bool) {
	var notes []Finding
	add := func(conf, f string, a ...any) {
		notes = append(notes, Finding{Field: "install", Confidence: conf, Note: fmt.Sprintf(f, a...)})
	}
	switch {
	case has(src, "package.json"):
		b, _ := src.Read("package.json")
		var pkg struct {
			Main    string            `json:"main"`
			Scripts map[string]string `json:"scripts"`
			Engines map[string]string `json:"engines"`
		}
		_ = json.Unmarshal(b, &pkg)
		tool, install, run := "npm", "npm install", "npm start"
		switch {
		case has(src, "pnpm-lock.yaml"):
			tool, install, run = "pnpm", "pnpm install --frozen-lockfile", "pnpm start"
		case has(src, "yarn.lock"):
			tool, install, run = "yarn", "yarn install --frozen-lockfile", "yarn start"
		case has(src, "package-lock.json"):
			install = "npm ci"
		}
		setup := install
		if _, ok := pkg.Scripts["build"]; ok && !strings.Contains(pkg.Scripts["prepare"], "build") {
			setup += " && " + tool + " run build"
		}
		opt := library.InstallOption{Type: "git", Requires: []string{"node", tool}, Setup: setup}
		switch {
		case pkg.Scripts["start"] != "":
			opt.Launch = run
			add("detected", "Node.js project: package.json has a start script (%s).", pkg.Scripts["start"])
		case pkg.Main != "":
			opt.Launch = "node " + pkg.Main
			add("guessed", "Node.js project without a start script; starts its main file %s.", pkg.Main)
		default:
			opt.Launch = run
			add("guessed", "Node.js project without a start script or main file; check the start command.")
		}
		if v := pkg.Engines["node"]; v != "" {
			add("detected", "Needs Node %s (MyDailyApps only checks that node exists, not its version).", v)
		}
		return opt, notes, true

	case has(src, "go.mod"):
		b, _ := src.Read("go.mod")
		name := goModuleName(string(b))
		setup := "go build -o ."
		if !hasGoMain(src, "") {
			if cmds := goCmds(src); len(cmds) > 0 {
				name = cmds[0]
				setup = "go build -o . ./cmd/" + name
			}
		}
		if name == "" {
			return library.InstallOption{}, nil, false
		}
		add("detected", "Go project (module %s); builds one executable.", name)
		return library.InstallOption{Type: "git", Requires: []string{"go"}, Setup: setup, Launch: name}, notes, true

	case has(src, "Cargo.toml"):
		b, _ := src.Read("Cargo.toml")
		name := tomlValue(string(b), "package", "name")
		if name == "" {
			return library.InstallOption{}, nil, false
		}
		add("detected", "Rust project (crate %s).", name)
		return library.InstallOption{Type: "git", Requires: []string{"cargo"}, Setup: "cargo build --release", Launch: "target/release/" + name}, notes, true

	case has(src, "pyproject.toml") || has(src, "requirements.txt"):
		opt := library.InstallOption{Type: "git", Requires: []string{"python"}}
		if has(src, "requirements.txt") {
			opt.Setup = "python -m pip install -r requirements.txt"
		} else {
			opt.Setup = "python -m pip install ."
		}
		if has(src, "pyproject.toml") {
			b, _ := src.Read("pyproject.toml")
			if script := tomlFirstKey(string(b), "project.scripts"); script != "" && opt.Setup == "python -m pip install ." {
				opt.Launch = script
			}
		}
		if opt.Launch == "" {
			for _, f := range []string{"app.py", "main.py", "server.py", "run.py"} {
				if has(src, f) {
					opt.Launch = "python " + f
					break
				}
			}
		}
		if opt.Launch == "" {
			opt.Launch = "python main.py"
			add("guessed", "Python project; no app.py/main.py found, check the start command.")
		} else {
			add("guessed", "Python project; starts with %q.", opt.Launch)
		}
		add("guessed", "Installs packages into the computer's Python; a virtual environment may be cleaner.")
		return opt, notes, true
	}
	return library.InstallOption{}, nil, false
}

func goModuleName(gomod string) string {
	for _, line := range strings.Split(gomod, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			parts := strings.Split(strings.Trim(strings.TrimSpace(line[7:]), `"`), "/")
			last := parts[len(parts)-1]
			if regexp.MustCompile(`^v\d+$`).MatchString(last) && len(parts) > 1 {
				last = parts[len(parts)-2]
			}
			return last
		}
	}
	return ""
}

func hasGoMain(src Source, dir string) bool {
	for _, f := range under(src, dir) {
		if strings.HasSuffix(f, ".go") && !strings.HasSuffix(f, "_test.go") && path.Base(f) == "main.go" {
			return true
		}
	}
	return false
}

func goCmds(src Source) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range src.Files() {
		parts := strings.Split(f, "/")
		if len(parts) == 3 && parts[0] == "cmd" && parts[2] == "main.go" && !seen[parts[1]] {
			seen[parts[1]] = true
			out = append(out, parts[1])
		}
	}
	return out
}

// tomlValue reads key = "value" inside [section] (a tiny subset of TOML).
func tomlValue(doc, section, key string) string {
	in := false
	for _, line := range strings.Split(doc, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			in = strings.Trim(line, "[] ") == section
			continue
		}
		if in {
			k, v, ok := strings.Cut(line, "=")
			if ok && strings.TrimSpace(k) == key {
				return strings.Trim(strings.TrimSpace(v), `"'`)
			}
		}
	}
	return ""
}

func tomlFirstKey(doc, section string) string {
	in := false
	for _, line := range strings.Split(doc, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			in = strings.Trim(line, "[] ") == section
			continue
		}
		if in && line != "" && !strings.HasPrefix(line, "#") {
			if k, _, ok := strings.Cut(line, "="); ok {
				return strings.Trim(strings.TrimSpace(k), `"'`)
			}
		}
	}
	return ""
}

// --- source scan ---------------------------------------------------------------

type scanResult struct {
	readsPort    bool
	portWhere    string
	fixedPort    int
	readsDataDir bool
	dataWhere    string
}

var (
	rePortEnv = regexp.MustCompile(`process\.env\.PORT|process\.env\[["']PORT["']\]|os\.Getenv\("PORT"\)|LookupEnv\("PORT"\)|os\.environ(\.get)?\s*[\(\[]\s*["']PORT|env::var\("PORT"\)|getenv\(\s*["']PORT`)
	reDataEnv = regexp.MustCompile(`process\.env\.DATA_DIR|process\.env\[["']DATA_DIR["']\]|os\.Getenv\("DATA_DIR"\)|LookupEnv\("DATA_DIR"\)|os\.environ(\.get)?\s*[\(\[]\s*["']DATA_DIR|env::var\("DATA_DIR"\)|getenv\(\s*["']DATA_DIR|kit\.Load\(`)
	reListen  = regexp.MustCompile(`(?i)(?:\.listen\(\s*|port\s*[:=]\s*|["']:)(\d{4,5})\b`)
)

var codeExt = map[string]bool{".js": true, ".mjs": true, ".cjs": true, ".ts": true, ".mts": true, ".go": true, ".py": true, ".rs": true}

// candidates picks the source files most likely to hold server setup.
func candidates(src Source, n int) []string {
	type scored struct {
		p     string
		score int
	}
	var list []scored
	for _, f := range src.Files() {
		if !codeExt[path.Ext(f)] {
			continue
		}
		lf := strings.ToLower(f)
		if strings.Contains(lf, "test") || strings.Contains(lf, "spec") || strings.HasPrefix(lf, "dist/") ||
			strings.HasPrefix(lf, "build/") || strings.HasPrefix(lf, "public/") || strings.Contains(lf, ".config.") ||
			strings.HasSuffix(lf, ".d.ts") {
			continue
		}
		base := strings.TrimSuffix(path.Base(lf), path.Ext(lf))
		score := strings.Count(f, "/") * 2
		switch base {
		case "server", "main", "index", "app", "config", "settings", "env":
			score -= 10
		}
		if strings.Contains(lf, "server") || strings.HasPrefix(lf, "cmd/") {
			score -= 5
		}
		if strings.Contains(lf, "client") || strings.Contains(lf, "component") || strings.Contains(lf, "/ui/") {
			score += 8
		}
		list = append(list, scored{f, score})
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].score < list[j].score })
	var out []string
	for i := 0; i < len(list) && i < n; i++ {
		out = append(out, list[i].p)
	}
	return out
}

func scanSource(src Source) scanResult {
	var r scanResult
	for _, f := range candidates(src, 8) {
		b, err := src.Read(f)
		if err != nil {
			continue
		}
		text := string(b)
		if !r.readsPort && rePortEnv.MatchString(text) {
			r.readsPort, r.portWhere = true, f
		}
		if !r.readsDataDir && reDataEnv.MatchString(text) {
			r.readsDataDir, r.dataWhere = true, f
		}
		if r.fixedPort == 0 {
			if m := reListen.FindStringSubmatch(text); m != nil {
				if p, _ := strconv.Atoi(m[1]); p >= 1024 && p <= 65535 {
					r.fixedPort = p
					if r.portWhere == "" {
						r.portWhere = f
					}
				}
			}
		}
		if r.readsPort && r.readsDataDir {
			break
		}
	}
	if r.readsPort {
		r.fixedPort = 0
	}
	return r
}

// --- .gitignore -----------------------------------------------------------------

var dataNames = map[string]bool{"data": true, "saves": true, "save": true, "db": true, "database": true, "storage": true,
	"userdata": true, "user-data": true, "profiles": true, "uploads": true}
var dataExts = []string{".db", ".sqlite", ".sqlite3"}

// gitignoreData turns data-like .gitignore entries into sync patterns
// relative to the app folder.
func gitignoreData(src Source) []string {
	if !has(src, ".gitignore") {
		return nil
	}
	b, err := src.Read(".gitignore")
	if err != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		l := strings.TrimSpace(line)
		if l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, "!") {
			continue
		}
		name := strings.Trim(l, "/")
		pat := ""
		switch {
		case dataNames[strings.ToLower(name)]:
			pat = name + "/**"
		case strings.HasPrefix(name, "*") && contains(dataExts, strings.ToLower(path.Ext(name))):
			pat = "**/" + name
		}
		if pat != "" && !seen[pat] {
			seen[pat] = true
			out = append(out, pat)
		}
	}
	return out
}
