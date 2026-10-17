package procs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// HostState is the content of the state file.
type HostState struct {
	HostPID     int        `json:"hostPid"`
	HostStarted int64      `json:"hostStarted"`
	Port        int        `json:"port"`
	Mode        string     `json:"mode"`
	GuestDir    string     `json:"guestDir,omitempty"`
	Apps        []StateApp `json:"apps"`
}

// StateApp is one running app in the state file.
type StateApp struct {
	ID      string `json:"id"`
	PID     int    `json:"pid"`
	Started int64  `json:"started"` // unix nanos, to guard against PID reuse
}

// AlreadyRunningError means another host instance owns the state file.
type AlreadyRunningError struct{ Port, PID int }

func (e *AlreadyRunningError) Error() string {
	return fmt.Sprintf("MyDailyApps is already running (process %d, http://localhost:%d)", e.PID, e.Port)
}

// StateFile is the on-disk record of this host and its apps.
type StateFile struct {
	path string
	mu   sync.Mutex
	st   HostState
}

// DefaultStatePath is a fixed location so both modes and all data folders
// share one lock: a second instance is refused whatever its settings.
func DefaultStatePath() string {
	return filepath.Join(os.TempDir(), "mydailyapps-host.json")
}

// AcquireState takes over the state file. If another live host owns it an
// *AlreadyRunningError is returned. If a stale file from a crashed session is
// found, it is returned as previous so the caller can clean up.
func AcquireState(path string, st HostState) (sf *StateFile, previous *HostState, err error) {
	if b, err := os.ReadFile(path); err == nil {
		var old HostState
		if json.Unmarshal(b, &old) == nil && old.HostPID != 0 {
			if old.HostPID != os.Getpid() && IsAlive(old.HostPID) && sameProcess(old.HostPID, old.HostStarted) {
				return nil, nil, &AlreadyRunningError{Port: old.Port, PID: old.HostPID}
			}
			previous = &old
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	st.HostPID = os.Getpid()
	st.HostStarted = processStartTime(os.Getpid())
	if st.HostStarted == 0 {
		st.HostStarted = time.Now().UnixNano()
	}
	sf = &StateFile{path: path, st: st}
	if err := sf.write(); err != nil {
		return nil, nil, err
	}
	return sf, previous, nil
}

func (s *StateFile) SetApps(apps []StateApp) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Apps = apps
	return s.write()
}

func (s *StateFile) write() error {
	b, _ := json.MarshalIndent(s.st, "", "  ")
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Remove deletes the state file at clean exit.
func (s *StateFile) Remove() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return os.Remove(s.path)
}

// CleanupLeftovers kills apps left running by a crashed session. Each PID is
// checked against its recorded start time so an unrelated process that
// reused the PID is not touched. Returns the ids that were stopped.
func CleanupLeftovers(prev *HostState) []string {
	var stopped []string
	for _, a := range prev.Apps {
		if a.PID <= 0 || !leftoverAlive(a) {
			continue
		}
		if KillTree(a.PID) == nil {
			stopped = append(stopped, a.ID)
		}
	}
	return stopped
}

// sameProcess reports whether pid still is the process started at started
// (unix nanos). Unknown start times are trusted.
func sameProcess(pid int, started int64) bool {
	if started == 0 {
		return true
	}
	actual := processStartTime(pid)
	if actual == 0 { // start time not available on this OS
		return true
	}
	d := time.Duration(actual - started)
	if d < 0 {
		d = -d
	}
	return d < 5*time.Second
}

// SetGuestDir records the guest temp folder so a crashed session's folder can
// be removed at the next start.
func (s *StateFile) SetGuestDir(dir string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.GuestDir = dir
	return s.write()
}
