// Package config loads MyDailyApps settings from environment variables and an
// optional mydailyapps.env file next to the executable, and prepares the data
// folders for guest or persistent mode.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Mode is guest (temporary, wiped on exit) or persistent.
type Mode string

const (
	ModeGuest      Mode = "guest"
	ModePersistent Mode = "persistent"
)

// EnvFileName is read from the executable's folder if present.
const EnvFileName = "mydailyapps.env"

// GuestPrefix names guest temp folders so leftovers can be found and removed.
const GuestPrefix = "mydailyapps-guest-"

// Config is the resolved configuration for one run.
type Config struct {
	Mode        Mode
	DataDir     string // local state, caches, tokens
	AppsDir     string // scanned for installed apps; install target
	Port        int
	OpenBrowser bool
	ExeDir      string

	// Google OAuth client for the Drive connection (desktop app type).
	GoogleClientID     string
	GoogleClientSecret string

	// GuestDir is the temp folder that holds everything in guest mode.
	GuestDir string
}

// Build-time defaults, set with -ldflags "-X ...". Lets a release binary ship
// with the OAuth client so users do not have to configure one.
var (
	DefaultGoogleClientID     = ""
	DefaultGoogleClientSecret = ""
)

// Load reads the env file and environment. It does not create folders.
func Load() (*Config, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	exeDir := filepath.Dir(exe)

	fileVars, err := readEnvFile(filepath.Join(exeDir, EnvFileName))
	if err != nil {
		return nil, err
	}
	get := func(name string) string {
		if v, ok := os.LookupEnv(name); ok { // environment wins over the file
			return strings.TrimSpace(v)
		}
		return fileVars[name]
	}

	c := &Config{ExeDir: exeDir, Port: 8080, OpenBrowser: true, Mode: ModeGuest}

	switch m := strings.ToLower(get("MYDAILYAPPS_MODE")); m {
	case "", "guest":
		c.Mode = ModeGuest
	case "persistent":
		c.Mode = ModePersistent
	default:
		return nil, fmt.Errorf("MYDAILYAPPS_MODE must be guest or persistent, got %q", m)
	}

	if v := get("MYDAILYAPPS_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p <= 0 || p > 65535 {
			return nil, fmt.Errorf("MYDAILYAPPS_PORT is not a valid port: %q", v)
		}
		c.Port = p
	}
	if v := get("MYDAILYAPPS_OPEN_BROWSER"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("MYDAILYAPPS_OPEN_BROWSER must be true or false, got %q", v)
		}
		c.OpenBrowser = b
	}

	c.DataDir = get("MYDAILYAPPS_DATA_DIR")
	c.AppsDir = get("MYDAILYAPPS_APPS_DIR")

	c.GoogleClientID = firstNonEmpty(get("MYDAILYAPPS_GOOGLE_CLIENT_ID"), DefaultGoogleClientID)
	c.GoogleClientSecret = firstNonEmpty(get("MYDAILYAPPS_GOOGLE_CLIENT_SECRET"), DefaultGoogleClientSecret)
	return c, nil
}

// Prepare creates the folders for this run. In guest mode it creates a fresh
// temp folder; the caller must call Cleanup at exit.
func (c *Config) Prepare() error {
	if c.Mode == ModeGuest {
		dir, err := os.MkdirTemp("", GuestPrefix)
		if err != nil {
			return err
		}
		c.GuestDir = dir
		// Mark the owner so a later run can tell whether this folder is stale.
		_ = os.WriteFile(filepath.Join(dir, "owner.pid"), []byte(strconv.Itoa(os.Getpid())), 0o600)
		// In guest mode everything lives in the temp folder, even if a data dir
		// was configured: guest mode must leave nothing behind.
		c.DataDir = filepath.Join(dir, "data")
		if c.AppsDir == "" {
			c.AppsDir = filepath.Join(dir, "apps")
		}
	} else {
		if c.DataDir == "" {
			base, err := os.UserConfigDir()
			if err != nil {
				return fmt.Errorf("cannot find the app-data folder, set MYDAILYAPPS_DATA_DIR: %w", err)
			}
			c.DataDir = filepath.Join(base, "MyDailyApps")
		}
		if c.AppsDir == "" {
			c.AppsDir = filepath.Join(c.ExeDir, "apps")
		}
	}
	var err error
	if c.DataDir, err = filepath.Abs(c.DataDir); err != nil {
		return err
	}
	if c.AppsDir, err = filepath.Abs(c.AppsDir); err != nil {
		return err
	}
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return err
	}
	return os.MkdirAll(c.AppsDir, 0o755)
}

// AppDataDir is the DATA_DIR given to one app.
func (c *Config) AppDataDir(appID string) string {
	return filepath.Join(c.DataDir, "appdata", appID)
}

// Cleanup removes the guest temp folder. No-op in persistent mode.
func (c *Config) Cleanup() error {
	if c.Mode != ModeGuest || c.GuestDir == "" {
		return nil
	}
	return RemoveAllRetry(c.GuestDir)
}

// RemoveAllRetry retries because on Windows files can stay locked for a
// moment after the processes using them were killed.
func RemoveAllRetry(dir string) error {
	var err error
	for i := 0; i < 10; i++ {
		if err = os.RemoveAll(dir); err == nil {
			return nil
		}
		sleep(300)
	}
	return err
}

// CleanGuestLeftovers removes guest folders from earlier runs whose owner
// process is gone (for example after a crash). isAlive reports whether a PID
// still runs.
func CleanGuestLeftovers(isAlive func(pid int) bool) []string {
	matches, _ := filepath.Glob(filepath.Join(os.TempDir(), GuestPrefix+"*"))
	var removed []string
	for _, dir := range matches {
		b, err := os.ReadFile(filepath.Join(dir, "owner.pid"))
		if err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid != os.Getpid() && isAlive(pid) {
				continue
			}
		}
		if RemoveAllRetry(dir) == nil {
			removed = append(removed, dir)
		}
	}
	return removed
}

func readEnvFile(path string) (map[string]string, error) {
	vars := map[string]string{}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return vars, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		s = strings.TrimPrefix(s, "export ")
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			return nil, fmt.Errorf("%s line %d: expected NAME=value", path, line)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		vars[strings.TrimSpace(k)] = v
	}
	return vars, sc.Err()
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
