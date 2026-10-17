package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnvFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, EnvFileName)
	os.WriteFile(p, []byte("# comment\nMYDAILYAPPS_PORT=9000\nexport MYDAILYAPPS_MODE=\"persistent\"\n"), 0o600)
	v, err := readEnvFile(p)
	if err != nil || v["MYDAILYAPPS_PORT"] != "9000" || v["MYDAILYAPPS_MODE"] != "persistent" {
		t.Fatalf("%v %v", v, err)
	}
}

func TestGuestCleanup(t *testing.T) {
	t.Setenv("MYDAILYAPPS_MODE", "guest")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Prepare(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.AppsDir); err != nil {
		t.Fatal(err)
	}
	if err := c.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.GuestDir); !os.IsNotExist(err) {
		t.Fatal("guest folder still exists")
	}
}
