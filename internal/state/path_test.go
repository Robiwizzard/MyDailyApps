package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOddFolderNames(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "John Doe #1 100%")
	os.MkdirAll(dir, 0o700)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SetSetting("a", "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.db")); err != nil {
		t.Fatal("database not created at the expected path:", err)
	}
}
