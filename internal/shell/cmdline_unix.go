//go:build !windows

package shell

import "os/exec"

func setCmdLine(c *exec.Cmd, line string) {}
