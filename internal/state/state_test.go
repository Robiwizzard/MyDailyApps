package state

import "testing"

func TestStateSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.SetInstallPath("app", "/x")
	s.SetSetting("k", "v")
	s.Close()
	s, _ = Open(dir)
	defer s.Close()
	a, _ := s.App("app")
	if a.InstallPath != "/x" {
		t.Fatal("install path lost")
	}
	if v, _ := s.Setting("k"); v != "v" {
		t.Fatal("setting lost")
	}
}
