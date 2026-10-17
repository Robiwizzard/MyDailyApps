// Package host is the core of MyDailyApps: it ties the library, the folder
// scan, installs, running apps, settings and Google Drive together. The HTTP
// layer in internal/api is a thin wrapper around it.
package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/robiwizzard/mydailyapps/internal/cloudsync"
	"github.com/robiwizzard/mydailyapps/internal/config"
	"github.com/robiwizzard/mydailyapps/internal/drive"
	"github.com/robiwizzard/mydailyapps/internal/install"
	"github.com/robiwizzard/mydailyapps/internal/library"
	"github.com/robiwizzard/mydailyapps/internal/procs"
	"github.com/robiwizzard/mydailyapps/internal/shell"
	"github.com/robiwizzard/mydailyapps/internal/state"
)

// Step is one startup step shown on the loading screen.
type Step struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	State  string `json:"state"` // pending, running, done, error, skipped
	Detail string `json:"detail,omitempty"`
}

// Host is the running host.
type Host struct {
	Cfg   *config.Config
	Store *state.Store
	Lib   *library.Cache
	Drive *drive.Service
	Procs *procs.Manager
	Sync  *cloudsync.Syncer

	mu              sync.Mutex
	resolved        map[string]library.App // library entry merged with the app's manifest
	scan            map[string]install.ScanResult
	syncLocks       map[string]*sync.Mutex
	syncing         map[string]bool
	syncNote        map[string]string
	syncWG          sync.WaitGroup
	updates         map[string]install.UpdateStatus
	updating        map[string]bool
	checkingUpdates bool
	installing      map[string]bool
	installErr      map[string]string
	steps           []Step
	ready           bool
	notices         []string
	libNote         string
	tokens          map[string]string // session-only GitHub tokens by repo owner
	version         int64             // bumps on any change, lets the UI poll cheaply

	shutdownOnce sync.Once
	ShutdownCh   chan struct{}
}

// New builds a host. Call Startup next.
func New(cfg *config.Config, store *state.Store, sf *procs.StateFile) (*Host, error) {
	h := &Host{
		Cfg: cfg, Store: store, Lib: library.NewCache(cfg.DataDir),
		resolved: map[string]library.App{}, syncLocks: map[string]*sync.Mutex{}, syncing: map[string]bool{}, syncNote: map[string]string{},
		updates: map[string]install.UpdateStatus{}, updating: map[string]bool{},
		scan: map[string]install.ScanResult{}, installing: map[string]bool{}, installErr: map[string]string{},
		tokens: map[string]string{}, ShutdownCh: make(chan struct{}),
		steps: []Step{
			{ID: "signin", Label: "Signing in to Google Drive", State: "pending"},
			{ID: "library", Label: "Fetching the library", State: "pending"},
			{ID: "manifests", Label: "Reading app manifests", State: "pending"},
			{ID: "scan", Label: "Scanning installed apps", State: "pending"},
		},
	}
	redirect := fmt.Sprintf("http://127.0.0.1:%d/api/drive/callback", cfg.Port)
	h.Drive = drive.New(cfg.GoogleClientID, cfg.GoogleClientSecret, redirect, tokenStore{store})
	m, err := procs.NewManager(sf, h.bump)
	if err != nil {
		return nil, err
	}
	h.Procs = m
	h.Sync = &cloudsync.Syncer{Remote: h.Drive, Records: syncRecords{store}, Checkpoint: state.CheckpointSQLite}
	return h, nil
}

func (h *Host) bump() {
	h.mu.Lock()
	h.version++
	h.mu.Unlock()
}

// AddNotice shows a message on the loading screen and library page.
func (h *Host) AddNotice(msg string) {
	h.mu.Lock()
	h.notices = append(h.notices, msg)
	h.mu.Unlock()
	h.bump()
}

func (h *Host) setStep(id, st, detail string) {
	h.mu.Lock()
	for i := range h.steps {
		if h.steps[i].ID == id {
			h.steps[i].State, h.steps[i].Detail = st, detail
		}
	}
	h.mu.Unlock()
	h.bump()
}

// Status is the GET /api/status payload.
type Status struct {
	Ready           bool     `json:"ready"`
	CheckingUpdates bool     `json:"checkingUpdates"`
	Steps           []Step   `json:"steps"`
	Notices         []string `json:"notices"`
	Mode            string   `json:"mode"`
	Version         int64    `json:"version"`
	Library         string   `json:"library"` // where the library came from
}

func (h *Host) Status() Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, src := h.Lib.Get()
	if h.libNote != "" {
		src += " (" + h.libNote + ")"
	}
	return Status{Ready: h.ready, CheckingUpdates: h.checkingUpdates, Steps: append([]Step(nil), h.steps...), Notices: append([]string{}, h.notices...),
		Mode: string(h.Cfg.Mode), Version: h.version, Library: src}
}

// Startup signs in, loads the library and scans the apps folder.
// It never installs anything.
func (h *Host) Startup(ctx context.Context) {
	// Library from local cache first, so the list shows even offline.
	if err := h.Lib.LoadLocal(filepath.Join(h.Cfg.ExeDir, library.FileName)); err != nil {
		h.AddNotice("The cached library file could not be read: " + err.Error())
	}

	switch {
	case !h.Drive.Configured():
		h.setStep("signin", "skipped", "Google sign-in is not configured")
	case !h.Drive.Connected():
		h.setStep("signin", "skipped", "Not connected (connect in the profile panel)")
	default:
		h.setStep("signin", "running", "")
		if err := h.Drive.Hello(ctx); err != nil {
			h.setStep("signin", "error", "Drive unreachable, using the cached library: "+err.Error())
		} else {
			h.setStep("signin", "done", h.Drive.Email())
		}
	}

	h.setStep("library", "running", "")
	if h.Drive.Connected() {
		if err := h.SyncLibraryFromDrive(ctx); err != nil {
			h.mu.Lock()
			h.libNote = "Drive unreachable, using cached copy"
			h.mu.Unlock()
			h.setStep("library", "error", "Using the cached library: "+err.Error())
		} else {
			h.setStep("library", "done", h.libraryDetail())
		}
	} else {
		h.setStep("library", "done", h.libraryDetail())
	}

	h.setStep("manifests", "running", "")
	h.setStep("manifests", "done", h.RefreshManifests(ctx))

	h.setStep("scan", "running", h.Cfg.AppsDir)
	h.Rescan()
	h.setStep("scan", "done", h.scanDetail())

	h.mu.Lock()
	h.ready = true
	h.mu.Unlock()
	h.bump()

	// Look for app updates in the background; the library is usable meanwhile.
	go h.CheckUpdates(ctx)
}

func (h *Host) libraryDetail() string {
	l, src := h.Lib.Get()
	return fmt.Sprintf("%d apps (from %s)", len(l.Apps), src)
}

func (h *Host) scanDetail() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.scan {
		if r.Installed {
			n++
		}
	}
	return fmt.Sprintf("%d installed in %s", n, h.Cfg.AppsDir)
}

// SyncLibraryFromDrive downloads the library; if Drive has none yet, the
// local copy is uploaded so the next machine can restore it.
func (h *Host) SyncLibraryFromDrive(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	b, err := h.Drive.DownloadFile(ctx, library.FileName)
	if err != nil {
		return err
	}
	if b == nil {
		l, _ := h.Lib.Get()
		if len(l.Apps) > 0 {
			data, _ := l.Marshal()
			return h.Drive.UploadFile(ctx, library.FileName, "application/json", data)
		}
		return nil
	}
	l, err := library.Parse(b)
	if err != nil {
		return fmt.Errorf("the library file in Drive is invalid: %w", err)
	}
	if err := h.Lib.Set(l, "Google Drive"); err != nil {
		return err
	}
	h.mu.Lock()
	h.libNote = ""
	h.mu.Unlock()
	// Restore the profile picture if this machine has none.
	if pic, _ := h.Store.Setting("profile_picture"); pic == "" {
		if p, err := h.Drive.DownloadFile(ctx, pictureFile); err == nil && p != nil && validPicture(string(p)) == nil {
			_ = h.Store.SetSetting("profile_picture", string(p))
		}
	}
	h.bump()
	return nil
}

// Rescan checks the apps folder for every library app. Nothing is fetched.
func (h *Host) Rescan() {
	l, _ := h.Lib.Get()
	states, _ := h.Store.Apps()
	res := map[string]install.ScanResult{}
	resolved := map[string]library.App{}
	for _, e := range l.Apps {
		dir := h.folder(e.ID, states)
		a := library.Merge(e, h.manifestFor(e.ID, dir))
		resolved[e.ID] = a
		res[e.ID] = install.Scan(a, dir)
	}
	h.mu.Lock()
	h.scan = res
	h.resolved = resolved
	h.mu.Unlock()
	h.bump()
}

func (h *Host) folder(id string, states map[string]state.AppState) string {
	if states == nil {
		s, _ := h.Store.App(id)
		return install.Folder(h.Cfg.AppsDir, s.InstallPath, id)
	}
	return install.Folder(h.Cfg.AppsDir, states[id].InstallPath, id)
}

// AppView is one card in the UI.
type AppView struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Group        string `json:"group"`
	Description  string `json:"description,omitempty"`
	Repo         string `json:"repo,omitempty"`
	Status       string `json:"status"` // not_installed, installing, stopped, starting, running
	URL          string `json:"url,omitempty"`
	Folder       string `json:"folder"`
	LastLaunched string `json:"lastLaunched,omitempty"`
	Error        string `json:"error,omitempty"`
	Cloud        bool   `json:"cloud"`              // the app syncs data to Drive
	Syncing      bool   `json:"syncing,omitempty"`  // a sync is running now
	SyncNote     string `json:"syncNote,omitempty"` // result or problem of the last sync
	Update       string `json:"update,omitempty"`   // set when a newer version exists, e.g. "3 new commits"
	Updating     bool   `json:"updating,omitempty"`
}

// Apps merges the library with the scan and running state. Never installs.
func (h *Host) Apps() []AppView {
	l, _ := h.Lib.Get()
	states, _ := h.Store.Apps()
	h.mu.Lock()
	scan := h.scan
	installing := copyBools(h.installing)
	errs := copyStrings(h.installErr)
	resolved := h.resolved
	syncing := copyBools(h.syncing)
	notes := copyStrings(h.syncNote)
	updates := make(map[string]install.UpdateStatus, len(h.updates))
	for k, v := range h.updates {
		updates[k] = v
	}
	updating := copyBools(h.updating)
	h.mu.Unlock()
	out := make([]AppView, 0, len(l.Apps))
	for _, e := range l.Apps {
		a, ok := resolved[e.ID]
		if !ok {
			a = library.Merge(e, nil)
		}
		v := AppView{ID: a.ID, Name: a.Name, Group: a.Group, Description: a.Description, Repo: a.Repo,
			Folder: h.folder(a.ID, states), Error: errs[a.ID],
			Cloud: a.Cloud != nil, Syncing: syncing[a.ID], SyncNote: notes[a.ID], Updating: updating[a.ID]}
		if u := updates[a.ID]; u.Available {
			v.Update = u.Detail
			if v.Update == "" {
				v.Update = "new version"
			}
		}
		if t := states[a.ID].LastLaunched; !t.IsZero() {
			v.LastLaunched = t.Format(time.RFC3339)
		}
		st, url := h.Procs.Status(a.ID)
		switch {
		case installing[a.ID]:
			v.Status = "installing"
		case st != "":
			v.Status, v.URL = st, url
		case scan[a.ID].Installed:
			v.Status = "stopped"
		default:
			v.Status = "not_installed"
		}
		out = append(out, v)
	}
	return out
}

// app returns the library entry merged with the app's manifest.
func (h *Host) app(id string) (library.App, error) {
	l, _ := h.Lib.Get()
	e, ok := l.Find(id)
	if !ok {
		return e, ErrNotFound
	}
	s, _ := h.Store.App(id)
	return library.Merge(e, h.manifestFor(id, install.Folder(h.Cfg.AppsDir, s.InstallPath, id))), nil
}

// ErrNotFound means the id is not in the library.
var ErrNotFound = errors.New("no app with that id in the library")

// ConfirmError asks the UI to show commands and confirm before running them.
type ConfirmError struct {
	Kind     string   `json:"kind"` // install or launch
	Folder   string   `json:"folder"`
	Commands []string `json:"commands"`
	Launch   string   `json:"launch"`
	Cloud    []string `json:"cloud,omitempty"` // files synced to Drive
}

func (e *ConfirmError) Error() string { return "confirmation needed before running these commands" }

// TokenError asks the UI for a GitHub token.
type TokenError struct{ Message string }

func (e *TokenError) Error() string { return e.Message }

// MissingError lists tools that are needed but not found.
type MissingError struct {
	Missing  []string
	Problems []string
}

func (e *MissingError) Error() string {
	if len(e.Missing) > 0 {
		return "Needs " + strings.Join(e.Missing, ", ") + ", not found"
	}
	return strings.Join(e.Problems, "; ")
}

func (h *Host) tokenFor(repo, given string) string {
	key := repoOwner(repo)
	h.mu.Lock()
	defer h.mu.Unlock()
	if given != "" {
		h.tokens[key] = given // session memory only, never written to disk
		return given
	}
	return h.tokens[key]
}

func repoOwner(repo string) string {
	r := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(repo, "https://"), "http://"), ".git")
	parts := strings.Split(r, "/")
	if len(parts) >= 2 {
		return parts[0] + "/" + parts[1]
	}
	return r
}

func approvalKey(folder, launch string, extra ...string) string {
	return strings.Join(append([]string{"folder: " + folder, "launch: " + launch}, extra...), "\n")
}

// Install installs one app when the user clicks Install. Without confirm it
// returns a *ConfirmError with the exact commands to show first.
func (h *Host) Install(ctx context.Context, id string, confirm bool, token string, log func(string)) error {
	a, err := h.app(id)
	if err != nil {
		return err
	}
	s, _ := h.Store.App(id)
	dir := install.Folder(h.Cfg.AppsDir, s.InstallPath, id)
	if r := install.Scan(a, dir); r.Installed {
		// The folder is already there: treat it as installed, never clone over it.
		h.Rescan()
		return nil
	}
	h.mu.Lock()
	if h.installing[id] {
		h.mu.Unlock()
		return errors.New("already installing")
	}
	h.installing[id] = true
	delete(h.installErr, id)
	h.mu.Unlock()
	h.bump()
	defer func() {
		h.mu.Lock()
		delete(h.installing, id)
		h.mu.Unlock()
		h.Rescan()
	}()

	opt := install.Options{Token: h.tokenFor(a.Repo, token)}
	if len(a.Install) == 0 || token != "" {
		// No install options yet: read them from the repo's manifest.
		if err := h.fetchManifest(ctx, a, opt.Token); errors.Is(err, install.ErrNeedsToken) {
			return &TokenError{Message: "This app's repository looks private. Enter a GitHub token to install it (used for this session only)."}
		}
		if a, err = h.app(id); err != nil {
			return err
		}
	}
	plan, err := install.MakePlan(ctx, a, dir, opt)
	if errors.Is(err, install.ErrNeedsToken) {
		return &TokenError{Message: "This app's repository or release looks private. Enter a GitHub token to install it (used for this session only)."}
	}
	if err != nil {
		return err
	}
	if plan.Option < 0 {
		return &MissingError{Missing: plan.Missing, Problems: plan.Problems}
	}
	if !confirm {
		return &ConfirmError{Kind: "install", Folder: dir, Commands: plan.Commands, Launch: plan.Launch, Cloud: cloudList(a.Cloud)}
	}
	err = install.Run(ctx, a, plan, opt, log)
	if errors.Is(err, install.ErrNeedsToken) {
		return &TokenError{Message: "The repository needs a GitHub token (it may be private). The token is used for this session only."}
	}
	if err != nil {
		h.mu.Lock()
		h.installErr[id] = err.Error()
		h.mu.Unlock()
		return err
	}
	// The user saw and confirmed the launch command (and synced files) too.
	_ = h.Store.SetApproved(id, approvalKey(dir, plan.Launch, cloudList(h.manifestMerged(id, dir).Cloud)...))
	return nil
}

// Launch starts an installed app and returns the URL once its port answers.
func (h *Host) Launch(ctx context.Context, id string, confirm bool) (string, error) {
	a, err := h.app(id)
	if err != nil {
		return "", err
	}
	s, _ := h.Store.App(id)
	dir := install.Folder(h.Cfg.AppsDir, s.InstallPath, id)
	r := install.Scan(a, dir)
	if !r.Installed {
		h.Rescan()
		return "", errors.New("the app is not installed")
	}
	if r.Launch == "" {
		return "", errors.New("the library entry has no launch command")
	}
	key := approvalKey(dir, r.Launch, cloudList(a.Cloud)...)
	if s.Approved != key {
		if !confirm {
			return "", &ConfirmError{Kind: "launch", Folder: dir, Commands: []string{r.Launch}, Launch: r.Launch, Cloud: cloudList(a.Cloud)}
		}
		_ = h.Store.SetApproved(id, key)
	}
	dataDir := h.Cfg.AppDataDir(id)
	_ = os.MkdirAll(dataDir, 0o700)
	if st, _ := h.Procs.Status(id); st == "" {
		// Steam-style: bring newer cloud files down before the app starts.
		h.syncApp(ctx, id, dataDir, a.Cloud, pull)
	}
	cloud := a.Cloud
	url, err := h.Procs.Start(ctx, procs.Spec{
		ID: id, Dir: dir, Command: r.Launch, Port: a.Port, Path: a.Path,
		Env: []string{"DATA_DIR=" + dataDir, "MYDAILYAPPS_MODE=" + string(h.Cfg.Mode), "MYDAILYAPPS_HOST=http://localhost:" + fmt.Sprint(h.Cfg.Port)},
		// After the app exits, upload what changed.
		OnExit: func() { h.syncAppAsync(id, dataDir, cloud, push) },
	})
	if err != nil {
		return "", err
	}
	_ = h.Store.SetLastLaunched(id, time.Now())
	h.bump()
	return url, nil
}

// LaunchCommandExists reports whether the program in the launch command can
// be found (used for clearer errors).
func LaunchCommandExists(dir, launch string) bool {
	return shell.LocalProgram(dir, launch) != "" || shell.Has(shell.FirstToken(launch))
}

func (h *Host) Stop(id string, force bool) error {
	if _, err := h.app(id); err != nil {
		return err
	}
	return h.Procs.Stop(id, force)
}

// ImportLibrary replaces the library (and saves it to Drive if connected).
func (h *Host) ImportLibrary(ctx context.Context, b []byte) error {
	l, err := library.Parse(b)
	if err != nil {
		return err
	}
	if err := h.Lib.Set(l, "imported file"); err != nil {
		return err
	}
	h.Rescan()
	if h.Drive.Connected() {
		data, _ := l.Marshal()
		ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		if err := h.Drive.UploadFile(ctx, library.FileName, "application/json", data); err != nil {
			return fmt.Errorf("saved locally, but saving to Drive failed: %w", err)
		}
	}
	return nil
}

// RequestShutdown starts the shutdown path once.
func (h *Host) RequestShutdown() {
	h.shutdownOnce.Do(func() { close(h.ShutdownCh) })
}

// Sorted helper for groups.
func (h *Host) Groups() []string {
	h.mu.Lock()
	resolved := h.resolved
	h.mu.Unlock()
	seen := map[string]bool{}
	out := []string{}
	for _, a := range resolved {
		if a.Group != "" && !seen[a.Group] {
			seen[a.Group] = true
			out = append(out, a.Group)
		}
	}
	sort.Strings(out)
	return out
}

// tokenStore keeps the Drive token in local state only.
type tokenStore struct{ s *state.Store }

func (t tokenStore) LoadToken() (*oauth2.Token, error) {
	v, err := t.s.Setting("drive_token")
	if err != nil || v == "" {
		return nil, err
	}
	var tok oauth2.Token
	if err := json.Unmarshal([]byte(v), &tok); err != nil {
		return nil, err
	}
	return &tok, nil
}

func (t tokenStore) SaveToken(tok *oauth2.Token) error {
	if tok == nil {
		return t.s.SetSetting("drive_token", "")
	}
	b, err := json.Marshal(tok)
	if err != nil {
		return err
	}
	return t.s.SetSetting("drive_token", string(b))
}
