// Package install resolves app folders, scans which apps are installed, checks
// requirements and installs apps from a release package or a git clone.
// Nothing here runs unless the user clicks Install (the scan only looks).
package install

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/robiwizzard/mydailyapps/internal/library"
	"github.com/robiwizzard/mydailyapps/internal/shell"
)

// MarkerName is written into an app folder after a successful install.
const MarkerName = ".mydailyapps-installed.json"

// Marker records how an app was installed.
type Marker struct {
	Option      int       `json:"option"` // index into the library entry's install list
	Type        string    `json:"type"`
	Launch      string    `json:"launch"`
	InstalledAt time.Time `json:"installedAt"`
}

// ErrNeedsToken means the repo or release looks private: ask for a token.
var ErrNeedsToken = errors.New("this repository needs a token (it may be private)")

// Folder returns the app's folder: the custom path if set, else <appsDir>/<id>.
func Folder(appsDir, customPath, id string) string {
	if customPath != "" {
		return customPath
	}
	return filepath.Join(appsDir, id)
}

// ScanResult describes one app's local install status.
type ScanResult struct {
	Installed bool
	Option    int    // which install option the installed copy uses (-1 unknown)
	Launch    string // launch command to use
}

// Scan checks one app folder. An app counts as installed when the folder
// exists and either the host's marker file is there or a launch target of
// one of its install options is present.
func Scan(app library.App, dir string) ScanResult {
	res := ScanResult{Option: -1}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return res
	}
	if m, err := ReadMarker(dir); err == nil {
		res.Installed = true
		res.Option, res.Launch = m.Option, m.Launch
		if m.Option >= 0 && m.Option < len(app.Install) {
			res.Launch = app.Install[m.Option].Launch // library may have updated it
		}
		return res
	}
	empty := isEmptyDir(dir)
	for i, o := range app.Install {
		if shell.LocalProgram(dir, o.Launch) != "" || (!empty && shell.Has(shell.FirstToken(o.Launch))) {
			res.Installed, res.Option, res.Launch = true, i, o.Launch
			return res
		}
	}
	// The folder exists but nothing matches. Treat it as installed (never
	// clone over an existing folder); launching will report the problem.
	if !empty {
		res.Installed = true
		if len(app.Install) > 0 {
			res.Option, res.Launch = 0, app.Install[0].Launch
		}
	}
	return res
}

func ReadMarker(dir string) (Marker, error) {
	var m Marker
	b, err := os.ReadFile(filepath.Join(dir, MarkerName))
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(b, &m)
	return m, err
}

func writeMarker(dir string, m Marker) error {
	b, _ := json.MarshalIndent(m, "", "  ")
	return os.WriteFile(filepath.Join(dir, MarkerName), b, 0o644)
}

func isEmptyDir(dir string) bool {
	f, err := os.Open(dir)
	if err != nil {
		return true
	}
	defer f.Close()
	names, _ := f.Readdirnames(1)
	return len(names) == 0
}

// Plan is what an install would do, shown to the user before it runs.
type Plan struct {
	Option   int      `json:"option"`
	Type     string   `json:"type"`
	Folder   string   `json:"folder"`
	Commands []string `json:"commands"` // exact steps, in order
	Launch   string   `json:"launch"`
	Missing  []string `json:"missing,omitempty"` // when no option works
	Problems []string `json:"problems,omitempty"`
	release  *releaseAsset
}

// Options for planning and installing.
type Options struct {
	Token string // GitHub token for private repos (session only, never saved)
}

// MakePlan picks the first install option that works on this machine.
func MakePlan(ctx context.Context, app library.App, dir string, opt Options) (*Plan, error) {
	if app.Repo == "" {
		return &Plan{Option: -1, Folder: dir, Problems: []string{"the library entry has no repo to install from"}}, nil
	}
	if len(app.Install) == 0 {
		return &Plan{Option: -1, Folder: dir, Problems: []string{"no install options: add a mydailyapps.json to the app's repository, or install options to its library entry"}}, nil
	}
	var problems, missing []string
	needsToken := false
	for i, o := range app.Install {
		var miss []string
		for _, r := range o.Requires {
			if !shell.Has(r) {
				miss = append(miss, r)
			}
		}
		if o.Type == "git" && !shell.Has("git") {
			miss = append(miss, "git")
		}
		if len(miss) > 0 {
			missing = append(missing, miss...)
			problems = append(problems, fmt.Sprintf("%s option: needs %s, not found", o.Type, strings.Join(miss, ", ")))
			continue
		}
		p := &Plan{Option: i, Type: o.Type, Folder: dir, Launch: o.Launch}
		switch o.Type {
		case "release":
			a, err := findRelease(ctx, app.Repo, o, opt.Token)
			if err != nil {
				if errors.Is(err, ErrNeedsToken) {
					needsToken = true
				}
				problems = append(problems, "release: "+err.Error())
				continue
			}
			p.release = a
			p.Commands = append(p.Commands, fmt.Sprintf("download %s", a.URL), fmt.Sprintf("unpack into %s", dir))
		case "git":
			p.Commands = append(p.Commands, fmt.Sprintf("git clone %s %s", app.Repo, quoteArg(dir)))
		}
		if o.Setup != "" {
			p.Commands = append(p.Commands, o.Setup)
		}
		return p, nil
	}
	p := &Plan{Option: -1, Folder: dir, Missing: dedupe(missing), Problems: problems}
	if needsToken && len(p.Missing) == 0 {
		return p, ErrNeedsToken
	}
	return p, nil
}

// Run performs the plan. The folder must not exist yet. log receives output.
func Run(ctx context.Context, app library.App, p *Plan, opt Options, log func(string)) error {
	if p.Option < 0 {
		return errors.New("no install option works on this machine")
	}
	if _, err := os.Stat(p.Folder); err == nil {
		return fmt.Errorf("folder %s already exists", p.Folder)
	}
	if err := os.MkdirAll(filepath.Dir(p.Folder), 0o755); err != nil {
		return err
	}
	o := app.Install[p.Option]
	ok := false
	defer func() {
		if !ok { // never leave a half-installed folder that would look installed
			_ = os.RemoveAll(p.Folder)
		}
	}()

	switch o.Type {
	case "git":
		if err := gitClone(ctx, app.Repo, p.Folder, opt.Token, log); err != nil {
			return err
		}
	case "release":
		if p.release == nil {
			return errors.New("no release package selected")
		}
		if err := downloadAndUnpack(ctx, p.release, p.Folder, shell.FirstToken(o.Launch), opt.Token, log); err != nil {
			return err
		}
	}
	if o.Setup != "" {
		log("$ " + o.Setup)
		if err := runLogged(shell.Command(ctx, p.Folder, o.Setup), log); err != nil {
			return fmt.Errorf("setup command failed: %w", err)
		}
	}
	if err := writeMarker(p.Folder, Marker{Option: p.Option, Type: o.Type, Launch: o.Launch, InstalledAt: time.Now()}); err != nil {
		return err
	}
	ok = true
	return nil
}

func gitClone(ctx context.Context, repo, dir, token string, log func(string)) error {
	log(fmt.Sprintf("$ git clone %s %s", repo, dir))
	c := exec.CommandContext(ctx, "git", "clone", repo, dir)
	c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if token != "" {
		// Passed through the environment, not the command line or git config.
		auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		c.Env = append(c.Env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader",
			"GIT_CONFIG_VALUE_0=Authorization: Basic "+auth)
	}
	var buf bytes.Buffer
	err := runLogged(c, func(s string) { buf.WriteString(s + "\n"); log(s) })
	if err != nil {
		out := strings.ToLower(buf.String())
		if strings.Contains(out, "could not read username") || strings.Contains(out, "authentication failed") ||
			strings.Contains(out, "repository not found") || strings.Contains(out, "terminal prompts disabled") {
			return ErrNeedsToken
		}
		return fmt.Errorf("git clone failed: %w", err)
	}
	return nil
}

func runLogged(c *exec.Cmd, log func(string)) error {
	w := &lineWriter{fn: log}
	c.Stdout, c.Stderr = w, w
	err := c.Run()
	w.Flush()
	return err
}

type lineWriter struct {
	fn  func(string)
	buf []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexAny(w.buf, "\r\n")
		if i < 0 {
			break
		}
		if line := strings.TrimSpace(string(w.buf[:i])); line != "" {
			w.fn(line)
		}
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

func (w *lineWriter) Flush() {
	if s := strings.TrimSpace(string(w.buf)); s != "" {
		w.fn(s)
	}
	w.buf = nil
}

func quoteArg(s string) string {
	if strings.ContainsAny(s, " \t") {
		return `"` + s + `"`
	}
	return s
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
