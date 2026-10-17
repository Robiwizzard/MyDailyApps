package install

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/robiwizzard/mydailyapps/internal/library"
)

type releaseAsset struct {
	Name   string
	URL    string // browser download URL (public)
	APIURL string // API URL (works with a token for private repos)
	Size   int64
}

var httpClient = &http.Client{Timeout: 10 * time.Minute}

// APIBase is the GitHub API root (overridable in tests).
var APIBase = "https://api.github.com"

// githubRepo extracts owner/repo from a GitHub URL.
func githubRepo(repo string) (string, error) {
	u, err := url.Parse(strings.TrimSuffix(repo, ".git"))
	if err != nil || u.Host != "github.com" {
		return "", errors.New("release installs are supported for github.com repositories")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return "", fmt.Errorf("cannot read owner/repo from %s", repo)
	}
	return parts[0] + "/" + parts[1], nil
}

func findRelease(ctx context.Context, repo string, o library.InstallOption, token string) (*releaseAsset, error) {
	or, err := githubRepo(repo)
	if err != nil {
		return nil, err
	}
	endpoint := APIBase + "/repos/" + or + "/releases/latest"
	if o.Tag != "" {
		endpoint = APIBase + "/repos/" + or + "/releases/tags/" + url.PathEscape(o.Tag)
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	ctx2, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, err := httpClient.Do(req.WithContext(ctx2))
	if err != nil {
		return nil, fmt.Errorf("cannot reach GitHub: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == 404 && token == "":
		return nil, fmt.Errorf("no release found (if the repo is private, a token is needed): %w", ErrNeedsToken)
	case resp.StatusCode == 404:
		return nil, errors.New("no release found")
	case rateLimited(resp):
		return nil, errRateLimited
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return nil, fmt.Errorf("GitHub refused the request (%s): %w", resp.Status, ErrNeedsToken)
	case resp.StatusCode != 200:
		return nil, fmt.Errorf("GitHub answered %s", resp.Status)
	}
	var rel struct {
		Assets []struct {
			Name               string `json:"name"`
			URL                string `json:"url"`
			BrowserDownloadURL string `json:"browser_download_url"`
			Size               int64  `json:"size"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	var names []string
	for _, a := range rel.Assets {
		names = append(names, a.Name)
		if matchesPlatform(a.Name, o.Asset, runtime.GOOS, runtime.GOARCH) {
			return &releaseAsset{Name: a.Name, URL: a.BrowserDownloadURL, APIURL: a.URL, Size: a.Size}, nil
		}
	}
	return nil, fmt.Errorf("no package for %s/%s in the release (has: %s)", runtime.GOOS, runtime.GOARCH, strings.Join(names, ", "))
}

var osWords = map[string][]string{
	"windows": {"windows", "win64", "win32", "win"},
	"darwin":  {"darwin", "macos", "mac", "osx"},
	"linux":   {"linux"},
}
var archWords = map[string][]string{
	"amd64": {"amd64", "x86_64", "x64", "win64"},
	"arm64": {"arm64", "aarch64"},
	"386":   {"386", "i386", "x86", "win32"},
}

func matchesPlatform(name, want, goos, goarch string) bool {
	n := strings.ReplaceAll(strings.ToLower(name), "x86_64", "amd64")
	n = strings.ReplaceAll(n, "x86-64", "amd64")
	for _, skip := range []string{".sha256", ".sha512", ".sig", ".asc", ".txt", ".md", "checksums"} {
		if strings.Contains(n, skip) {
			return false
		}
	}
	if want != "" && !strings.Contains(n, strings.ToLower(want)) {
		return false
	}
	return containsWord(n, osWords[goos]) && containsWord(n, archWords[goarch])
}

func containsWord(name string, words []string) bool {
	tokens := strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' || r == '.' || r == ' ' })
	for _, t := range tokens {
		for _, w := range words {
			if t == w {
				return true
			}
		}
	}
	return false
}

func downloadAndUnpack(ctx context.Context, a *releaseAsset, dir, launchName, token string, log func(string)) error {
	log("downloading " + a.Name)
	u := a.URL
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	if token != "" && a.APIURL != "" {
		req, _ = http.NewRequestWithContext(ctx, "GET", a.APIURL, nil)
		req.Header.Set("Accept", "application/octet-stream")
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if !strings.HasPrefix(req.URL.String(), "https://") {
		return errors.New("refusing to download over plain HTTP")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download failed: %s", resp.Status)
	}
	tmp, err := os.CreateTemp("", "mydailyapps-dl-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	n, err := io.Copy(tmp, resp.Body)
	if err != nil {
		return err
	}
	log(fmt.Sprintf("downloaded %d KB, unpacking", n/1024))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	name := strings.ToLower(a.Name)
	switch {
	case strings.HasSuffix(name, ".zip"):
		return unzip(tmp.Name(), dir)
	case strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tgz"):
		if _, err := tmp.Seek(0, 0); err != nil {
			return err
		}
		return untar(tmp, dir)
	default:
		// A bare executable: save it under the launch name.
		target := launchName
		if runtime.GOOS == "windows" && filepath.Ext(target) == "" {
			target += ".exe"
		}
		if _, err := tmp.Seek(0, 0); err != nil {
			return err
		}
		out, err := os.OpenFile(filepath.Join(dir, filepath.Base(target)), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, tmp)
		return err
	}
}

// safeJoin protects against archive entries escaping the folder.
func safeJoin(dir, name string) (string, error) {
	p := filepath.Join(dir, filepath.FromSlash(name))
	rel, err := filepath.Rel(dir, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry %q is outside the target folder", name)
	}
	return p, nil
}

// commonRoot returns the single top-level folder all entries share, if any,
// so "app-v1/..." archives unpack straight into the app folder.
func commonRoot(names []string) string {
	root, nested := "", false
	for _, n := range names {
		n = strings.TrimPrefix(filepath.ToSlash(n), "./")
		if n == "" {
			continue
		}
		first, rest, found := strings.Cut(n, "/")
		if !found { // a file at the top level
			return ""
		}
		if root == "" {
			root = first
		} else if root != first {
			return ""
		}
		if rest != "" {
			nested = true
		}
	}
	if root == "" || !nested {
		return ""
	}
	return root + "/"
}

func unzip(path, dir string) error {
	r, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer r.Close()
	var names []string
	for _, f := range r.File {
		names = append(names, f.Name)
	}
	strip := commonRoot(names)
	for _, f := range r.File {
		name := strings.TrimPrefix(strings.TrimPrefix(filepath.ToSlash(f.Name), "./"), strip)
		if name == "" {
			continue
		}
		p, err := safeJoin(dir, name)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		mode := f.Mode().Perm() | 0o600
		if mode&0o111 == 0 && looksExecutable(name) {
			mode |= 0o755
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = writeFile(p, rc, mode)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func untar(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	// Read entries twice is not possible on a stream, so buffer names first
	// by extracting into a staging folder, then move.
	stage, err := os.MkdirTemp(filepath.Dir(dir), ".unpack-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		p, err := safeJoin(stage, h.Name)
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			names = append(names, h.Name)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			if err := writeFile(p, tr, os.FileMode(h.Mode).Perm()|0o600); err != nil {
				return err
			}
		}
	}
	src := stage
	if root := commonRoot(names); root != "" {
		src = filepath.Join(stage, strings.TrimSuffix(root, "/"))
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.Rename(filepath.Join(src, e.Name()), filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func writeFile(p string, r io.Reader, mode os.FileMode) error {
	out, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func looksExecutable(name string) bool {
	return filepath.Ext(name) == "" && !strings.Contains(filepath.Base(name), ".")
}
