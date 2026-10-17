package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/robiwizzard/mydailyapps/internal/library"
	"github.com/robiwizzard/mydailyapps/internal/shell"
)

// Update checks only look (git fetch, or ask GitHub for the newest release).
// Nothing in an app folder changes until the user clicks Update.

// UpdateStatus says whether a newer version is available.
type UpdateStatus struct {
	Available bool
	Kind      string // "git" or "release"
	Detail    string // e.g. "3 new commits" or "v1.2.0 → v1.3.0"
}

// ErrLocalChanges means a git-installed app has edits that a pull could overwrite.
var ErrLocalChanges = errors.New("the app folder has local changes; commit or undo them before updating")

func isGitRepo(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && st.IsDir()
}

func gitOut(ctx context.Context, dir, token string, args ...string) (string, error) {
	c := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	c.Env = gitEnv(token)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if needsAuth(msg) {
			return "", ErrNeedsToken
		}
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", args[0], msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// CheckUpdate looks for a newer version of an installed app.
func CheckUpdate(ctx context.Context, app library.App, dir, token string) (UpdateStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if isGitRepo(dir) {
		if !shell.Has("git") {
			return UpdateStatus{}, errors.New("git is not installed")
		}
		if _, err := gitOut(ctx, dir, token, "fetch", "--quiet"); err != nil {
			return UpdateStatus{}, err
		}
		n, err := gitOut(ctx, dir, token, "rev-list", "--count", "HEAD..@{u}")
		if err != nil {
			return UpdateStatus{}, nil // no upstream branch: nothing to compare with
		}
		behind, _ := strconv.Atoi(n)
		st := UpdateStatus{Kind: "git", Available: behind > 0}
		if behind == 1 {
			st.Detail = "1 new commit"
		} else if behind > 1 {
			st.Detail = fmt.Sprintf("%d new commits", behind)
		}
		return st, nil
	}
	m, err := ReadMarker(dir)
	if err != nil || m.Type != "release" || m.ReleaseTag == "" || m.Option < 0 || m.Option >= len(app.Install) {
		return UpdateStatus{}, nil // installed by hand, or by an older version of the host
	}
	o := app.Install[m.Option]
	if o.Tag != "" {
		return UpdateStatus{}, nil // pinned to one release
	}
	a, err := findRelease(ctx, app.Repo, o, token)
	if err != nil {
		return UpdateStatus{}, err
	}
	if a.Tag == "" || a.Tag == m.ReleaseTag {
		return UpdateStatus{Kind: "release"}, nil
	}
	return UpdateStatus{Kind: "release", Available: true, Detail: m.ReleaseTag + " → " + a.Tag}, nil
}

// UpdateCommands lists what Update will run, for the confirmation dialog.
// setup is the setup command the app will use after the update.
func UpdateCommands(app library.App, dir, setup string) []string {
	var cmds []string
	if isGitRepo(dir) {
		cmds = append(cmds, "git pull --ff-only")
	} else {
		cmds = append(cmds, "download the newest release package", "replace the files in "+dir)
	}
	if setup != "" {
		cmds = append(cmds, setup)
	}
	return cmds
}

// Update brings an installed, stopped app to the newest version.
// setupAfter is called once the new files are in place and returns the
// setup command to run (the app's manifest may have changed it).
func Update(ctx context.Context, app library.App, dir, token string, setupAfter func() string, log func(string)) error {
	if isGitRepo(dir) {
		return updateGit(ctx, dir, token, setupAfter, log)
	}
	m, err := ReadMarker(dir)
	if err != nil || m.Type != "release" || m.Option < 0 || m.Option >= len(app.Install) {
		return errors.New("this app was not installed by MyDailyApps from git or a release, so it cannot be updated here")
	}
	return updateRelease(ctx, app, m, dir, token, setupAfter, log)
}

func updateGit(ctx context.Context, dir, token string, setupAfter func() string, log func(string)) error {
	dirty, err := gitOut(ctx, dir, token, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return err
	}
	if dirty != "" {
		return ErrLocalChanges
	}
	log("$ git pull --ff-only")
	c := exec.CommandContext(ctx, "git", "-C", dir, "pull", "--ff-only")
	c.Env = gitEnv(token)
	var buf bytes.Buffer
	if err := runLogged(c, func(s string) { buf.WriteString(s + "\n"); log(s) }); err != nil {
		if needsAuth(buf.String()) {
			return ErrNeedsToken
		}
		return fmt.Errorf("git pull failed (the branch may have diverged): %w", err)
	}
	if err := runSetup(ctx, dir, setupAfter(), log); err != nil {
		return err
	}
	return touchMarker(dir, "")
}

func updateRelease(ctx context.Context, app library.App, m Marker, dir, token string, setupAfter func() string, log func(string)) error {
	o := app.Install[m.Option]
	a, err := findRelease(ctx, app.Repo, o, token)
	if err != nil {
		return err
	}
	// Unpack next to the app, then swap, so a failed update leaves the old version.
	staging := dir + ".update"
	_ = os.RemoveAll(staging)
	if err := downloadAndUnpack(ctx, a, staging, shell.FirstToken(o.Launch), token, log); err != nil {
		os.RemoveAll(staging)
		return err
	}
	if err := runSetup(ctx, staging, setupAfter(), log); err != nil {
		os.RemoveAll(staging)
		return err
	}
	m.ReleaseTag, m.UpdatedAt = a.Tag, time.Now()
	if err := writeMarker(staging, m); err != nil {
		os.RemoveAll(staging)
		return err
	}
	old := dir + ".old"
	_ = os.RemoveAll(old)
	if err := os.Rename(dir, old); err != nil {
		os.RemoveAll(staging)
		return fmt.Errorf("could not replace the app folder (is a file in it still open?): %w", err)
	}
	if err := os.Rename(staging, dir); err != nil {
		_ = os.Rename(old, dir) // put the old version back
		os.RemoveAll(staging)
		return err
	}
	_ = os.RemoveAll(old)
	log("updated to " + a.Tag)
	return nil
}

func runSetup(ctx context.Context, dir, setup string, log func(string)) error {
	if setup == "" {
		return nil
	}
	log("$ " + setup)
	if err := runLogged(shell.Command(ctx, dir, setup), log); err != nil {
		return fmt.Errorf("setup command failed: %w", err)
	}
	return nil
}

func touchMarker(dir, tag string) error {
	m, err := ReadMarker(dir)
	if err != nil {
		return nil // installed by hand: no marker to update
	}
	m.UpdatedAt = time.Now()
	if tag != "" {
		m.ReleaseTag = tag
	}
	return writeMarker(dir, m)
}
