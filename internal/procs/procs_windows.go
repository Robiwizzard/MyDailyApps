//go:build windows

package procs

import (
	"errors"
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Each app runs in its own job object. The job is created with
// KILL_ON_JOB_CLOSE, so if the host is killed (even with Task Manager) Windows
// closes the job handle and kills the app and everything it spawned.
type platformProc struct {
	job windows.Handle
}

type platformManager struct{}

func newPlatformManager() (platformManager, error) { return platformManager{}, nil }

func (platformManager) prepare(c *exec.Cmd) {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	// Own process group: lets us send CTRL_BREAK for a polite stop, and keeps
	// Ctrl+C in the host console from reaching the app before the host does.
	c.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
}

func (platformManager) attach(c *exec.Cmd) platformProc {
	job, err := newKillOnCloseJob()
	if err != nil {
		return platformProc{}
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(c.Process.Pid))
	if err != nil {
		windows.CloseHandle(job)
		return platformProc{}
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		windows.CloseHandle(job)
		return platformProc{}
	}
	return platformProc{job: job}
}

func newKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

func (platformManager) release(p *Proc) {
	if p.plat.job != 0 {
		windows.CloseHandle(p.plat.job) // kills anything still left in the job
		p.plat.job = 0
	}
}

// polite sends CTRL_BREAK to the app's process group (Node and Go apps treat
// it like Ctrl+C).
func (platformManager) polite(p *Proc) error {
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(p.PID))
}

func (platformManager) kill(p *Proc) {
	if p.plat.job != 0 {
		if windows.TerminateJobObject(p.plat.job, 1) == nil {
			return
		}
	}
	_ = KillTree(p.PID)
}

// KillTree kills a process and all its children.
func KillTree(pid int) error {
	if pid <= 0 {
		return errors.New("invalid pid")
	}
	c := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return c.Run()
}

const stillActive = 259

// IsAlive reports whether a process with the PID is running.
func IsAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// Access denied still means it exists.
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

func checkGroupLeader(pid int) bool { return true }

// processStartTime returns the process creation time in unix nanos, or 0.
func processStartTime(pid int) int64 {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0
	}
	defer windows.CloseHandle(h)
	var c, e, k, u windows.Filetime
	if err := windows.GetProcessTimes(h, &c, &e, &k, &u); err != nil {
		return 0
	}
	return c.Nanoseconds()
}

// leftoverAlive reports whether an app from an earlier session still runs.
// (Its children were in a kill-on-close job, so they died with the host.)
func leftoverAlive(a StateApp) bool {
	return IsAlive(a.PID) && sameProcess(a.PID, a.Started)
}
