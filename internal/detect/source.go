// Package detect drafts a library entry for an app by reading its repository
// (or installed folder) with fixed rules: no AI, no code is run. Every field
// it fills comes with a Finding that says how sure it is and why.
package detect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Source is what the rules can look at.
type Source interface {
	// Info returns repository metadata (may be partly empty).
	Info() RepoInfo
	// Files lists every file path (forward slashes), sorted.
	Files() []string
	// Read returns a file's content (small files only).
	Read(p string) ([]byte, error)
	// ReleaseAssets returns the file names of the newest release, if any.
	ReleaseAssets() []string
}

// RepoInfo is repository metadata.
type RepoInfo struct {
	Name        string
	Description string
	Topics      []string
}

// --- local folder -----------------------------------------------------------

type localSource struct {
	dir   string
	files []string
}

// skipDirs are never interesting and can be huge.
var skipDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true, "__pycache__": true, ".venv": true, "venv": true, "target": true}

// Local reads an app folder on this computer.
func Local(dir string) (Source, error) {
	s := &localSource{dir: dir}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != dir && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		s.files = append(s.files, filepath.ToSlash(rel))
		if len(s.files) > 20000 {
			return errors.New("too many files")
		}
		return nil
	})
	sort.Strings(s.files)
	return s, err
}

func (s *localSource) Info() RepoInfo          { return RepoInfo{Name: filepath.Base(s.dir)} }
func (s *localSource) Files() []string         { return s.files }
func (s *localSource) ReleaseAssets() []string { return nil }
func (s *localSource) Read(p string) ([]byte, error) {
	f, err := os.Open(filepath.Join(s.dir, filepath.FromSlash(p)))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 512<<10))
}

// --- GitHub -----------------------------------------------------------------

// APIBase is the GitHub API root (overridable in tests).
var APIBase = "https://api.github.com"

// HTTPClient is used for GitHub requests (overridable in tests).
var HTTPClient = &http.Client{Timeout: 20 * time.Second}

// ErrRateLimited means GitHub's limit for requests without a token was reached.
var ErrRateLimited = errors.New("GitHub's limit for requests without a token was reached; try again later or enter a GitHub token")

// ErrNotFound means the repository does not exist or is private.
var ErrNotFound = errors.New("repository not found (if it is private, enter a GitHub token)")

type githubSource struct {
	ctx    context.Context
	repo   string // owner/name
	ref    string
	token  string
	info   RepoInfo
	files  []string
	assets []string
	reads  int
}

// maxReads caps file downloads per detection to stay well inside GitHub's
// limit of 60 requests per hour without a token.
const maxReads = 14

// GitHub reads a repository through the GitHub API without cloning it.
func GitHub(ctx context.Context, repoURL, token string) (Source, error) {
	or, err := ownerRepo(repoURL)
	if err != nil {
		return nil, err
	}
	s := &githubSource{ctx: ctx, repo: or, token: token}
	var meta struct {
		Name          string   `json:"name"`
		Description   string   `json:"description"`
		Topics        []string `json:"topics"`
		DefaultBranch string   `json:"default_branch"`
	}
	if err := s.getJSON("/repos/"+or, &meta); err != nil {
		return nil, err
	}
	s.info = RepoInfo{Name: meta.Name, Description: meta.Description, Topics: meta.Topics}
	s.ref = meta.DefaultBranch
	var tree struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
	}
	if err := s.getJSON("/repos/"+or+"/git/trees/"+url.PathEscape(s.ref)+"?recursive=1", &tree); err != nil {
		return nil, err
	}
	for _, t := range tree.Tree {
		if t.Type != "blob" {
			continue
		}
		top := strings.SplitN(t.Path, "/", 2)[0]
		if skipDirs[top] {
			continue
		}
		s.files = append(s.files, t.Path)
	}
	sort.Strings(s.files)
	var rel struct {
		Assets []struct {
			Name string `json:"name"`
		} `json:"assets"`
	}
	if err := s.getJSON("/repos/"+or+"/releases/latest", &rel); err == nil {
		for _, a := range rel.Assets {
			s.assets = append(s.assets, a.Name)
		}
	} else if errors.Is(err, ErrRateLimited) {
		return nil, err
	}
	return s, nil
}

func (s *githubSource) Info() RepoInfo          { return s.info }
func (s *githubSource) Files() []string         { return s.files }
func (s *githubSource) ReleaseAssets() []string { return s.assets }

func (s *githubSource) Read(p string) ([]byte, error) {
	if s.reads >= maxReads {
		return nil, errors.New("read limit reached")
	}
	s.reads++
	resp, err := s.do("/repos/" + s.repo + "/contents/" + escapePath(p) + "?ref=" + url.QueryEscape(s.ref))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 512<<10))
}

func (s *githubSource) do(p string) (*http.Response, error) {
	req, _ := http.NewRequestWithContext(s.ctx, "GET", APIBase+p, nil)
	req.Header.Set("Accept", "application/vnd.github.raw+json")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach GitHub: %w", err)
	}
	switch {
	case resp.StatusCode == 200:
		return resp, nil
	case (resp.StatusCode == 403 || resp.StatusCode == 429) && resp.Header.Get("X-RateLimit-Remaining") == "0":
		resp.Body.Close()
		return nil, ErrRateLimited
	case resp.StatusCode == 404:
		resp.Body.Close()
		return nil, ErrNotFound
	default:
		resp.Body.Close()
		return nil, fmt.Errorf("GitHub answered %s", resp.Status)
	}
}

func (s *githubSource) getJSON(p string, out any) error {
	req, _ := http.NewRequestWithContext(s.ctx, "GET", APIBase+p, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach GitHub: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == 200:
		return json.NewDecoder(resp.Body).Decode(out)
	case (resp.StatusCode == 403 || resp.StatusCode == 429) && resp.Header.Get("X-RateLimit-Remaining") == "0":
		return ErrRateLimited
	case resp.StatusCode == 404:
		return ErrNotFound
	default:
		return fmt.Errorf("GitHub answered %s", resp.Status)
	}
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

func ownerRepo(repo string) (string, error) {
	u, err := url.Parse(strings.TrimSuffix(strings.TrimSpace(repo), ".git"))
	if err != nil || (u.Host != "github.com" && u.Host != "www.github.com") {
		return "", errors.New("detection reads repositories on github.com (paste a https://github.com/owner/name link)")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("cannot read owner/name from %s", repo)
	}
	return parts[0] + "/" + parts[1], nil
}

// has reports whether the source has a file at p.
func has(s Source, p string) bool {
	files := s.Files()
	i := sort.SearchStrings(files, p)
	return i < len(files) && files[i] == p
}

// under lists files whose directory is dir ("" = root).
func under(s Source, dir string) []string {
	var out []string
	for _, f := range s.Files() {
		if path.Dir(f) == dir || (dir == "" && !strings.Contains(f, "/")) {
			out = append(out, f)
		}
	}
	return out
}
