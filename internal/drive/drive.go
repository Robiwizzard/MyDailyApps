// Package drive signs in to Google (OAuth for installed apps, with a loopback
// redirect to the host) and reads and writes files in one Drive folder.
//
// Scope: drive.file — the host only sees files it created itself, never the
// rest of the user's Drive.
package drive

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

// Scope is the narrowest Drive scope that works for a library file.
const Scope = "https://www.googleapis.com/auth/drive.file"

// FolderName is the Drive folder that holds the host's files.
const FolderName = "MyDailyApps"

var googleEndpoint = oauth2.Endpoint{
	AuthURL:   "https://accounts.google.com/o/oauth2/auth",
	TokenURL:  "https://oauth2.googleapis.com/token",
	AuthStyle: oauth2.AuthStyleInParams,
}

// API roots (overridable in tests).
var (
	APIBase    = "https://www.googleapis.com/drive/v3"
	UploadBase = "https://www.googleapis.com/upload/drive/v3"
	RevokeURL  = "https://oauth2.googleapis.com/revoke"
)

// ErrNotConfigured means no OAuth client id was provided.
var ErrNotConfigured = errors.New("Google sign-in is not configured: set MYDAILYAPPS_GOOGLE_CLIENT_ID and MYDAILYAPPS_GOOGLE_CLIENT_SECRET")

// ErrNotConnected means the user has not signed in.
var ErrNotConnected = errors.New("Google Drive is not connected")

// TokenStore persists the token locally (never in the library file).
type TokenStore interface {
	LoadToken() (*oauth2.Token, error)
	SaveToken(*oauth2.Token) error // nil deletes
}

// Service manages sign-in and Drive calls.
type Service struct {
	conf  *oauth2.Config
	store TokenStore

	mu       sync.Mutex
	pending  map[string]string // state -> PKCE verifier
	folderID string
	email    string
}

// New returns a service. clientID may be empty (then Drive is unavailable).
func New(clientID, clientSecret, redirectURL string, store TokenStore) *Service {
	s := &Service{store: store, pending: map[string]string{}}
	if clientID != "" {
		s.conf = &oauth2.Config{
			ClientID: clientID, ClientSecret: clientSecret, Endpoint: googleEndpoint,
			RedirectURL: redirectURL, Scopes: []string{Scope},
		}
	}
	return s
}

func (s *Service) Configured() bool { return s.conf != nil }

// Connected reports whether a token is stored.
func (s *Service) Connected() bool {
	if s.conf == nil {
		return false
	}
	t, err := s.store.LoadToken()
	return err == nil && t != nil
}

// Email returns the signed-in account's address once known.
func (s *Service) Email() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.email
}

// AuthURL starts a sign-in and returns the Google URL to open.
func (s *Service) AuthURL() (string, error) {
	if s.conf == nil {
		return "", ErrNotConfigured
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	state := hex.EncodeToString(b)
	verifier := oauth2.GenerateVerifier()
	s.mu.Lock()
	s.pending[state] = verifier
	s.mu.Unlock()
	return s.conf.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("prompt", "consent")), nil
}

// HandleCallback finishes the sign-in from the redirect request.
func (s *Service) HandleCallback(ctx context.Context, q url.Values) error {
	if s.conf == nil {
		return ErrNotConfigured
	}
	if e := q.Get("error"); e != "" {
		return fmt.Errorf("Google sign-in was not completed: %s", e)
	}
	state := q.Get("state")
	s.mu.Lock()
	verifier, ok := s.pending[state]
	delete(s.pending, state)
	s.mu.Unlock()
	if !ok {
		return errors.New("sign-in link expired or unknown; please try again")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tok, err := s.conf.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(verifier))
	if err != nil {
		return fmt.Errorf("could not finish sign-in: %w", err)
	}
	return s.store.SaveToken(tok)
}

// Disconnect revokes the token (best effort) and deletes it locally.
func (s *Service) Disconnect(ctx context.Context) error {
	if t, err := s.store.LoadToken(); err == nil && t != nil {
		tok := t.RefreshToken
		if tok == "" {
			tok = t.AccessToken
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "POST", RevokeURL, strings.NewReader(url.Values{"token": {tok}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}
	s.mu.Lock()
	s.folderID, s.email = "", ""
	s.mu.Unlock()
	return s.store.SaveToken(nil)
}

// savingSource stores refreshed tokens.
type savingSource struct {
	base  oauth2.TokenSource
	store TokenStore
	mu    sync.Mutex
	last  string
}

func (s *savingSource) Token() (*oauth2.Token, error) {
	t, err := s.base.Token()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.AccessToken != s.last {
		s.last = t.AccessToken
		_ = s.store.SaveToken(t)
	}
	return t, nil
}

func (s *Service) client(ctx context.Context) (*http.Client, error) {
	if s.conf == nil {
		return nil, ErrNotConfigured
	}
	t, err := s.store.LoadToken()
	if err != nil || t == nil {
		return nil, ErrNotConnected
	}
	src := &savingSource{base: s.conf.TokenSource(context.Background(), t), store: s.store, last: t.AccessToken}
	c := oauth2.NewClient(ctx, oauth2.ReuseTokenSource(t, src))
	c.Timeout = 60 * time.Second
	return c, nil
}

type fileMeta struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	ModifiedTime string `json:"modifiedTime"`
}

func doJSON(c *http.Client, req *http.Request, out any) error {
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("Drive answered %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func escapeQ(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `'`, `\'`) }

func (s *Service) find(ctx context.Context, c *http.Client, q string) (*fileMeta, error) {
	u := APIBase + "/files?" + url.Values{
		"q": {q + " and trashed = false"}, "spaces": {"drive"},
		"fields": {"files(id,name,modifiedTime)"}, "orderBy": {"modifiedTime desc"}, "pageSize": {"10"},
	}.Encode()
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	var res struct {
		Files []fileMeta `json:"files"`
	}
	if err := doJSON(c, req, &res); err != nil {
		return nil, err
	}
	if len(res.Files) == 0 {
		return nil, nil
	}
	return &res.Files[0], nil
}

func (s *Service) folder(ctx context.Context, c *http.Client) (string, error) {
	s.mu.Lock()
	id := s.folderID
	s.mu.Unlock()
	if id != "" {
		return id, nil
	}
	f, err := s.find(ctx, c, fmt.Sprintf("name = '%s' and mimeType = 'application/vnd.google-apps.folder' and 'root' in parents", escapeQ(FolderName)))
	if err != nil {
		return "", err
	}
	if f == nil {
		body, _ := json.Marshal(map[string]any{"name": FolderName, "mimeType": "application/vnd.google-apps.folder"})
		req, _ := http.NewRequestWithContext(ctx, "POST", APIBase+"/files?fields=id", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		var created fileMeta
		if err := doJSON(c, req, &created); err != nil {
			return "", err
		}
		f = &created
	}
	s.mu.Lock()
	s.folderID = f.ID
	s.mu.Unlock()
	return f.ID, nil
}

// Hello checks the connection and learns the account email.
func (s *Service) Hello(ctx context.Context) error {
	c, err := s.client(ctx)
	if err != nil {
		return err
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", APIBase+"/about?fields=user(emailAddress)", nil)
	var res struct {
		User struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"user"`
	}
	if err := doJSON(c, req, &res); err != nil {
		return err
	}
	s.mu.Lock()
	s.email = res.User.EmailAddress
	s.mu.Unlock()
	return nil
}

// Download returns a file from the folder, or (nil, nil) if it does not exist.
func (s *Service) Download(ctx context.Context, name string) ([]byte, error) {
	c, err := s.client(ctx)
	if err != nil {
		return nil, err
	}
	folder, err := s.folder(ctx, c)
	if err != nil {
		return nil, err
	}
	f, err := s.find(ctx, c, fmt.Sprintf("name = '%s' and '%s' in parents", escapeQ(name), escapeQ(folder)))
	if err != nil || f == nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", APIBase+"/files/"+url.PathEscape(f.ID)+"?alt=media", nil)
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Drive answered %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 50<<20))
}

// Upload creates or replaces a file in the folder.
func (s *Service) Upload(ctx context.Context, name, mimeType string, data []byte) error {
	c, err := s.client(ctx)
	if err != nil {
		return err
	}
	folder, err := s.folder(ctx, c)
	if err != nil {
		return err
	}
	f, err := s.find(ctx, c, fmt.Sprintf("name = '%s' and '%s' in parents", escapeQ(name), escapeQ(folder)))
	if err != nil {
		return err
	}
	if f != nil {
		req, _ := http.NewRequestWithContext(ctx, "PATCH", UploadBase+"/files/"+url.PathEscape(f.ID)+"?uploadType=media", bytes.NewReader(data))
		req.Header.Set("Content-Type", mimeType)
		return doJSON(c, req, nil)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	meta, _ := json.Marshal(map[string]any{"name": name, "parents": []string{folder}})
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", "application/json; charset=UTF-8")
	pw, _ := mw.CreatePart(h)
	pw.Write(meta)
	h = textproto.MIMEHeader{}
	h.Set("Content-Type", mimeType)
	pw, _ = mw.CreatePart(h)
	pw.Write(data)
	mw.Close()
	req, _ := http.NewRequestWithContext(ctx, "POST", UploadBase+"/files?uploadType=multipart", &body)
	req.Header.Set("Content-Type", "multipart/related; boundary="+mw.Boundary())
	return doJSON(c, req, nil)
}
