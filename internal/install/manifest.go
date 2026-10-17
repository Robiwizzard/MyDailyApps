package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/robiwizzard/mydailyapps/internal/library"
)

// ErrNoManifest means the repository has no mydailyapps.json.
var ErrNoManifest = errors.New("no " + library.ManifestName + " in the repository")

// FetchManifest downloads mydailyapps.json from the default branch of a
// GitHub repository (before the app is installed). token may be empty.
func FetchManifest(ctx context.Context, repo, token string) ([]byte, error) {
	or, err := githubRepo(repo)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", APIBase+"/repos/"+or+"/contents/"+library.ManifestName, nil)
	req.Header.Set("Accept", "application/vnd.github.raw+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach GitHub: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == 404 && token == "":
		return nil, ErrNoManifest // or a private repo; the install step will ask for a token
	case resp.StatusCode == 404:
		return nil, ErrNoManifest
	case rateLimited(resp):
		return nil, errRateLimited
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return nil, fmt.Errorf("GitHub refused the request (%s): %w", resp.Status, ErrNeedsToken)
	case resp.StatusCode != 200:
		return nil, fmt.Errorf("GitHub answered %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// ReadManifest reads mydailyapps.json from an installed app folder.
// It returns (nil, nil) if the file does not exist.
func ReadManifest(dir string) (*library.App, error) {
	b, err := os.ReadFile(filepath.Join(dir, library.ManifestName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return library.ParseManifest(b)
}

var errRateLimited = errors.New("GitHub's limit for requests without a token was reached; try again in an hour, or enter a GitHub token")

func rateLimited(resp *http.Response) bool {
	return (resp.StatusCode == 403 || resp.StatusCode == 429) && resp.Header.Get("X-RateLimit-Remaining") == "0"
}
