// Package kit is a small shared library for apps launched by MyDailyApps.
//
// The app contract: read PORT and DATA_DIR from the environment and run on
// its own without the host. kit.Load does exactly that, with the same
// guest/persistent pattern the host uses when the app is started by hand.
package kit

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config is an app's runtime configuration.
type Config struct {
	AppID   string
	Port    int
	DataDir string
	Guest   bool // DataDir is temporary and removed by Cleanup
	// HostURL is set when the app was started by MyDailyApps.
	HostURL string
}

// Load reads PORT, DATA_DIR and MYDAILYAPPS_MODE.
//
//   - Started by the host: PORT and DATA_DIR are always set.
//   - Started by hand: PORT defaults to defaultPort; DATA_DIR defaults to a
//     temp folder in guest mode (the default) or <user config dir>/<appID>
//     when MYDAILYAPPS_MODE=persistent.
func Load(appID string, defaultPort int) (*Config, error) {
	c := &Config{AppID: appID, Port: defaultPort, HostURL: os.Getenv("MYDAILYAPPS_HOST")}
	if v := os.Getenv("PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p <= 0 || p > 65535 {
			return nil, fmt.Errorf("PORT is not a valid port: %q", v)
		}
		c.Port = p
	}
	c.DataDir = os.Getenv("DATA_DIR")
	if c.DataDir == "" {
		if strings.EqualFold(os.Getenv("MYDAILYAPPS_MODE"), "persistent") {
			base, err := os.UserConfigDir()
			if err != nil {
				return nil, err
			}
			c.DataDir = filepath.Join(base, appID)
		} else {
			dir, err := os.MkdirTemp("", appID+"-guest-")
			if err != nil {
				return nil, err
			}
			c.DataDir, c.Guest = dir, true
		}
	}
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return nil, err
	}
	return c, nil
}

// Addr is the listen address (localhost only).
func (c *Config) Addr() string { return "127.0.0.1:" + strconv.Itoa(c.Port) }

// Path returns a path inside DATA_DIR.
func (c *Config) Path(elem ...string) string {
	return filepath.Join(append([]string{c.DataDir}, elem...)...)
}

// Cleanup removes the data folder in guest mode. Call it at exit.
func (c *Config) Cleanup() error {
	if !c.Guest {
		return nil
	}
	return os.RemoveAll(c.DataDir)
}
