package kit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"golang.org/x/oauth2"
)

// DriveFileScope only gives access to files the app created itself.
const DriveFileScope = "https://www.googleapis.com/auth/drive.file"

// GoogleEndpoint is Google's OAuth endpoint.
var GoogleEndpoint = oauth2.Endpoint{
	AuthURL:   "https://accounts.google.com/o/oauth2/auth",
	TokenURL:  "https://oauth2.googleapis.com/token",
	AuthStyle: oauth2.AuthStyleInParams,
}

// GoogleConfig builds an OAuth config for a "Desktop app" client.
func GoogleConfig(clientID, clientSecret string) *oauth2.Config {
	return &oauth2.Config{ClientID: clientID, ClientSecret: clientSecret, Endpoint: GoogleEndpoint, Scopes: []string{DriveFileScope}}
}

// LoopbackLogin runs the installed-app sign-in: it listens on a random
// localhost port, calls open with the Google URL (open it in the browser),
// and waits for the redirect. Uses PKCE.
func LoopbackLogin(ctx context.Context, conf *oauth2.Config, open func(url string)) (*oauth2.Token, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	defer ln.Close()
	c := *conf
	c.RedirectURL = fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)

	b := make([]byte, 16)
	_, _ = rand.Read(b)
	state := hex.EncodeToString(b)
	verifier := oauth2.GenerateVerifier()

	type result struct {
		tok *oauth2.Token
		err error
	}
	done := make(chan result, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		var res result
		switch {
		case q.Get("state") != state:
			res.err = errors.New("sign-in state mismatch")
		case q.Get("error") != "":
			res.err = fmt.Errorf("sign-in was not completed: %s", q.Get("error"))
		default:
			res.tok, res.err = c.Exchange(r.Context(), q.Get("code"), oauth2.VerifierOption(verifier))
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if res.err != nil {
			fmt.Fprintf(w, "<p>Sign-in failed: %s</p>", res.err)
		} else {
			fmt.Fprint(w, "<p>Signed in. You can close this tab.</p>")
		}
		select {
		case done <- res:
		default:
		}
	})}
	go srv.Serve(ln)
	defer srv.Close()

	open(c.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("prompt", "consent")))
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	select {
	case r := <-done:
		return r.tok, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// FileTokenSource loads a token from path and saves refreshed tokens back.
// Keep path inside DATA_DIR; never sync it.
func FileTokenSource(ctx context.Context, conf *oauth2.Config, path string) (oauth2.TokenSource, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t oauth2.Token
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, err
	}
	return &savingSource{base: conf.TokenSource(ctx, &t), path: path, last: t.AccessToken}, nil
}

// SaveToken writes a token file (0600).
func SaveToken(path string, t *oauth2.Token) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

type savingSource struct {
	base oauth2.TokenSource
	path string
	last string
}

func (s *savingSource) Token() (*oauth2.Token, error) {
	t, err := s.base.Token()
	if err == nil && t.AccessToken != s.last {
		s.last = t.AccessToken
		_ = SaveToken(s.path, t)
	}
	return t, err
}
