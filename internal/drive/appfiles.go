package drive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/robiwizzard/mydailyapps/internal/cloudsync"
)

// App data lives in MyDailyApps/apps/<id>/ and conflict backups in
// MyDailyApps/backups/<id>/. Each synced file is stored flat in the app's
// folder with its relative path as the Drive file name; its checksum and
// original modification time are kept in appProperties (private to this
// OAuth client).

const folderMime = "application/vnd.google-apps.folder"

// child finds or creates a folder under parent (cached for the session).
func (s *Service) child(ctx context.Context, c *http.Client, parent, name string) (string, error) {
	key := parent + "/" + name
	s.mu.Lock()
	if id := s.children[key]; id != "" {
		s.mu.Unlock()
		return id, nil
	}
	s.mu.Unlock()
	f, err := s.find(ctx, c, fmt.Sprintf("name = '%s' and mimeType = '%s' and '%s' in parents", escapeQ(name), folderMime, escapeQ(parent)))
	if err != nil {
		return "", err
	}
	if f == nil {
		body, _ := json.Marshal(map[string]any{"name": name, "mimeType": folderMime, "parents": []string{parent}})
		req, _ := http.NewRequestWithContext(ctx, "POST", APIBase+"/files?fields=id", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		var created fileMeta
		if err := doJSON(c, req, &created); err != nil {
			return "", err
		}
		f = &created
	}
	s.mu.Lock()
	if s.children == nil {
		s.children = map[string]string{}
	}
	s.children[key] = f.ID
	s.mu.Unlock()
	return f.ID, nil
}

// appFolder returns MyDailyApps/<kind>/<appID>.
func (s *Service) appFolder(ctx context.Context, c *http.Client, kind, appID string) (string, error) {
	root, err := s.folder(ctx, c)
	if err != nil {
		return "", err
	}
	k, err := s.child(ctx, c, root, kind)
	if err != nil {
		return "", err
	}
	return s.child(ctx, c, k, appID)
}

type appFile struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Size          string            `json:"size"`
	ModifiedTime  string            `json:"modifiedTime"`
	AppProperties map[string]string `json:"appProperties"`
}

const appFileFields = "id,name,size,modifiedTime,appProperties"

func (f appFile) remote() cloudsync.RemoteFile {
	r := cloudsync.RemoteFile{ID: f.ID, Path: f.Name, SHA256: f.AppProperties["sha256"]}
	fmt.Sscan(f.Size, &r.Size)
	if t, err := time.Parse(time.RFC3339Nano, f.AppProperties["mtime"]); err == nil {
		r.Modified = t
	} else if t, err := time.Parse(time.RFC3339Nano, f.ModifiedTime); err == nil {
		r.Modified = t
	}
	return r
}

// List implements cloudsync.Remote.
func (s *Service) List(ctx context.Context, appID string) ([]cloudsync.RemoteFile, error) {
	c, err := s.client(ctx)
	if err != nil {
		return nil, err
	}
	folder, err := s.appFolder(ctx, c, "apps", appID)
	if err != nil {
		return nil, err
	}
	var out []cloudsync.RemoteFile
	page := ""
	for {
		v := url.Values{"q": {fmt.Sprintf("'%s' in parents and trashed = false and mimeType != '%s'", escapeQ(folder), folderMime)},
			"spaces": {"drive"}, "pageSize": {"1000"}, "fields": {"nextPageToken,files(" + appFileFields + ")"}}
		if page != "" {
			v.Set("pageToken", page)
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", APIBase+"/files?"+v.Encode(), nil)
		var res struct {
			NextPageToken string    `json:"nextPageToken"`
			Files         []appFile `json:"files"`
		}
		if err := doJSON(c, req, &res); err != nil {
			return nil, err
		}
		for _, f := range res.Files {
			if f.AppProperties["sha256"] == "" {
				continue // not a file this host synced
			}
			out = append(out, f.remote())
		}
		if res.NextPageToken == "" {
			return out, nil
		}
		page = res.NextPageToken
	}
}

// Download implements cloudsync.Remote.
func (s *Service) Download(ctx context.Context, fileID string, w io.Writer) error {
	c, err := s.client(ctx)
	if err != nil {
		return err
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", APIBase+"/files/"+url.PathEscape(fileID)+"?alt=media", nil)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return driveError(resp)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// upload sends metadata, then the content, with a resumable upload session
// (works for files of any size).
func upload(ctx context.Context, c *http.Client, existingID string, meta map[string]any, r io.Reader) (appFile, error) {
	var out appFile
	method, u := "POST", UploadBase+"/files?uploadType=resumable&fields="+appFileFields
	if existingID != "" {
		method, u = "PATCH", UploadBase+"/files/"+url.PathEscape(existingID)+"?uploadType=resumable&fields="+appFileFields
	}
	body, _ := json.Marshal(meta)
	req, _ := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	resp, err := c.Do(req)
	if err != nil {
		return out, err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return out, driveError(resp)
	}
	session := resp.Header.Get("Location")
	if session == "" {
		return out, errors.New("Drive did not return an upload session")
	}
	// Buffer to send an exact Content-Length (Drive requires it for a single PUT).
	data, err := io.ReadAll(r)
	if err != nil {
		return out, err
	}
	put, _ := http.NewRequestWithContext(ctx, "PUT", session, bytes.NewReader(data))
	put.Header.Set("Content-Type", "application/octet-stream")
	err = doJSON(c, put, &out)
	return out, err
}

// Upload implements cloudsync.Remote.
func (s *Service) Upload(ctx context.Context, appID, existingID string, f cloudsync.RemoteFile, r io.Reader) (cloudsync.RemoteFile, error) {
	c, err := s.client(ctx)
	if err != nil {
		return cloudsync.RemoteFile{}, err
	}
	meta := map[string]any{"appProperties": map[string]string{
		"sha256": f.SHA256, "mtime": f.Modified.UTC().Format(time.RFC3339Nano),
	}}
	if existingID == "" {
		folder, err := s.appFolder(ctx, c, "apps", appID)
		if err != nil {
			return cloudsync.RemoteFile{}, err
		}
		meta["name"], meta["parents"] = f.Path, []string{folder}
	}
	out, err := upload(ctx, c, existingID, meta, r)
	if err != nil {
		return cloudsync.RemoteFile{}, err
	}
	return out.remote(), nil
}

// BackupLocal implements cloudsync.Remote.
func (s *Service) BackupLocal(ctx context.Context, appID, name string, r io.Reader) error {
	c, err := s.client(ctx)
	if err != nil {
		return err
	}
	folder, err := s.appFolder(ctx, c, "backups", appID)
	if err != nil {
		return err
	}
	_, err = upload(ctx, c, "", map[string]any{"name": name, "parents": []string{folder}}, r)
	return err
}

// BackupRemote implements cloudsync.Remote: a server-side copy, no download.
func (s *Service) BackupRemote(ctx context.Context, appID, fileID, name string) error {
	c, err := s.client(ctx)
	if err != nil {
		return err
	}
	folder, err := s.appFolder(ctx, c, "backups", appID)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"name": name, "parents": []string{folder}, "appProperties": map[string]string{}})
	req, _ := http.NewRequestWithContext(ctx, "POST", APIBase+"/files/"+url.PathEscape(fileID)+"/copy?fields=id", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return doJSON(c, req, nil)
}

var _ cloudsync.Remote = (*Service)(nil)
