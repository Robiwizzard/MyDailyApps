//go:build !windows

package procs

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type platformProc struct{}
type platformManager struct{}

func newPlatformManager() (platformManager, error) { return platformManager{}, nil }

// prepare puts the app in its own process group so the whole tree (shell,
// npm, node...) can be signalled at once.
func (platformManager) prepare(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func (platformManager) attach(c *exec.Cmd) platformProc { return platformProc{} }
func (platformManager) release(p *Proc)                 {}

func (platformManager) polite(p *Proc) error {
	return syscall.Kill(-p.PID, syscall.SIGTERM)
}

func (platformManager) kill(p *Proc) {
	_ = syscall.Kill(-p.PID, syscall.SIGKILL)
}

// KillTree kills a process group left over from an earlier session.
func KillTree(pid int) error {
	if pid <= 1 {
		return errors.New("invalid pid")
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	for i := 0; i < 20; i++ {
		if syscall.Kill(-pid, 0) != nil { // whole group gone
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return syscall.Kill(-pid, syscall.SIGKILL)
}

// IsAlive reports whether a process with the PID exists.
func IsAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil || errors.Is(err, syscall.EPERM) {
		// Zombies count as dead.
		if runtime.GOOS == "linux" {
			if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
				if i := strings.LastIndexByte(string(b), ')'); i > 0 && i+2 < len(b) && b[i+2] == 'Z' {
					return false
				}
			}
		}
		return true
	}
	return false
}

// checkGroupLeader: apps were started as process group leaders.
func checkGroupLeader(pid int) bool {
	pg, err := syscall.Getpgid(pid)
	return err == nil && pg == pid
}

// processStartTime returns the start time in unix nanos (Linux only, else 0).
func processStartTime(pid int) int64 {
	if runtime.GOOS != "linux" {
		return 0
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 20 {
		return 0
	}
	ticks, err := strconv.ParseInt(f[19], 10, 64) // field 22: starttime
	if err != nil {
		return 0
	}
	st, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0
	}
	var btime int64
	for _, line := range strings.Split(string(st), "\n") {
		if strings.HasPrefix(line, "btime ") {
			btime, _ = strconv.ParseInt(strings.TrimSpace(line[6:]), 10, 64)
		}
	}
	if btime == 0 {
		return 0
	}
	const hz = 100 // USER_HZ is 100 on all mainstream Linux builds
	return btime*int64(time.Second) + ticks*int64(time.Second)/hz
}

// leftoverAlive reports whether an app from an earlier session still runs.
// The app was a process group leader; its children may outlive it.
func leftoverAlive(a StateApp) bool {
	if IsAlive(a.PID) {
		return sameProcess(a.PID, a.Started) && checkGroupLeader(a.PID)
	}
	return syscall.Kill(-a.PID, 0) == nil
}
