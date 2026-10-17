// Package shell builds OS shell commands and finds tools on the PATH.
package shell

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Command runs line through the platform shell (cmd /C or sh -c) in dir.
func Command(ctx context.Context, dir, line string) *exec.Cmd {
	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		c = exec.CommandContext(ctx, "cmd.exe")
		// Pass the line verbatim: cmd.exe has its own quoting rules.
		setCmdLine(c, `cmd.exe /D /S /C "`+line+`"`)
	} else {
		c = exec.CommandContext(ctx, "/bin/sh", "-c", line)
	}
	c.Dir = dir
	return c
}

// Has reports whether a tool can be found on the PATH.
func Has(tool string) bool {
	_, err := exec.LookPath(tool)
	return err == nil
}

// FirstToken returns the program part of a command line.
func FirstToken(line string) string {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, `"`) {
		if end := strings.Index(line[1:], `"`); end >= 0 {
			return line[1 : end+1]
		}
	}
	tok, _, _ := strings.Cut(line, " ")
	return tok
}

// LocalProgram returns the path of the program named by the command line if
// it is a file inside dir (adding .exe/.cmd/.bat on Windows), or "".
func LocalProgram(dir, line string) string {
	tok := FirstToken(line)
	if tok == "" {
		return ""
	}
	tok = strings.TrimPrefix(strings.TrimPrefix(tok, "./"), ".\\")
	cands := []string{tok}
	if runtime.GOOS == "windows" && filepath.Ext(tok) == "" {
		cands = append(cands, tok+".exe", tok+".cmd", tok+".bat")
	}
	for _, c := range cands {
		p := filepath.Join(dir, c)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// ResolveLaunch rewrites a command line so a program shipped in dir is run by
// its absolute path (sh does not search the current folder).
func ResolveLaunch(dir, line string) string {
	p := LocalProgram(dir, line)
	if p == "" {
		return line
	}
	rest := strings.TrimSpace(line)
	if strings.HasPrefix(rest, `"`) {
		if end := strings.Index(rest[1:], `"`); end >= 0 {
			rest = rest[end+2:]
		}
	} else {
		_, rest, _ = strings.Cut(rest, " ")
	}
	q := `"` + p + `"`
	if runtime.GOOS != "windows" {
		q = "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
	}
	return strings.TrimSpace(q + " " + rest)
}
