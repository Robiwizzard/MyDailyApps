package drive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// Each app's Drive folder (MyDailyApps/apps/<id>/) also holds a copy of the
// app's library entry in AppMetaName. If the app is removed from the library
// and added again later, or the library is lost, the host finds its earlier
// settings and data there. The file has no sha256 appProperty, so the file
// sync ignores it.

// AppMetaName is the settings copy inside an app's Drive folder.
const AppMetaName = ".mydailyapps-app.json"

// findChild finds a folder under parent without creating it ("" if missing).
func (s *Service) findChild(ctx context.Context, c *http.Client, parent, name string) (string, error) {
	f, err := s.find(ctx, c, fmt.Sprintf("name = '%s' and mimeType = '%s' and '%s' in parents", escapeQ(name), folderMime, escapeQ(parent)))
	if err != nil || f == nil {
		return "", err
	}
	return f.ID, nil
}

// existingAppFolder returns MyDailyApps/<kind>/<id> if it exists ("" if not).
func (s *Service) existingAppFolder(ctx context.Context, c *http.Client, kind, appID string) (string, error) {
	root, err := s.folder(ctx, c)
	if err != nil {
		return "", err
	}
	k, err := s.findChild(ctx, c, root, kind)
	if err != nil || k == "" {
		return "", err
	}
	return s.findChild(ctx, c, k, appID)
}

// SaveAppMeta stores the app's settings copy in its Drive folder.
func (s *Service) SaveAppMeta(ctx context.Context, appID string, data []byte) error {
	c, err := s.client(ctx)
	if err != nil {
		return err
	}
	folder, err := s.appFolder(ctx, c, "apps", appID)
	if err != nil {
		return err
	}
	f, err := s.find(ctx, c, fmt.Sprintf("name = '%s' and '%s' in parents", escapeQ(AppMetaName), escapeQ(folder)))
	if err != nil {
		return err
	}
	if f != nil {
		req, _ := http.NewRequestWithContext(ctx, "PATCH", UploadBase+"/files/"+url.PathEscape(f.ID)+"?uploadType=media", bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		return doJSON(c, req, nil)
	}
	_, err = upload(ctx, c, "", map[string]any{"name": AppMetaName, "parents": []string{folder}}, bytes.NewReader(data))
	return err
}

// AppFolderInfo describes what Drive already holds for an app.
type AppFolderInfo struct {
	Exists bool
	Meta   []byte   // the settings copy, if any
	Files  int      // synced files
	Paths  []string // their paths (relative to the app's sync root)
}

// AppFolder reports what Drive holds for an app, without creating anything.
func (s *Service) AppFolder(ctx context.Context, appID string) (AppFolderInfo, error) {
	var info AppFolderInfo
	c, err := s.client(ctx)
	if err != nil {
		return info, err
	}
	folder, err := s.existingAppFolder(ctx, c, "apps", appID)
	if err != nil || folder == "" {
		return info, err
	}
	info.Exists = true
	if f, err := s.find(ctx, c, fmt.Sprintf("name = '%s' and '%s' in parents", escapeQ(AppMetaName), escapeQ(folder))); err == nil && f != nil {
		req, _ := http.NewRequestWithContext(ctx, "GET", APIBase+"/files/"+url.PathEscape(f.ID)+"?alt=media", nil)
		if resp, err := c.Do(req); err == nil {
			if resp.StatusCode == 200 {
				info.Meta, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			}
			resp.Body.Close()
		}
	}
	// The folder is known to exist, so List does not create anything.
	if files, err := s.List(ctx, appID); err == nil {
		info.Files = len(files)
		for _, f := range files {
			info.Paths = append(info.Paths, f.Path)
		}
	}
	return info, nil
}

// AppFolderIDs lists the app ids that have a folder in MyDailyApps/apps/.
func (s *Service) AppFolderIDs(ctx context.Context) ([]string, error) {
	c, err := s.client(ctx)
	if err != nil {
		return nil, err
	}
	root, err := s.folder(ctx, c)
	if err != nil {
		return nil, err
	}
	apps, err := s.findChild(ctx, c, root, "apps")
	if err != nil || apps == "" {
		return nil, err
	}
	v := url.Values{"q": {fmt.Sprintf("'%s' in parents and mimeType = '%s' and trashed = false", escapeQ(apps), folderMime)},
		"spaces": {"drive"}, "pageSize": {"200"}, "fields": {"files(id,name)"}}
	req, _ := http.NewRequestWithContext(ctx, "GET", APIBase+"/files?"+v.Encode(), nil)
	var res struct {
		Files []fileMeta `json:"files"`
	}
	if err := doJSON(c, req, &res); err != nil {
		return nil, err
	}
	var ids []string
	for _, f := range res.Files {
		ids = append(ids, f.Name)
	}
	return ids, nil
}

// TrashApp moves an app's Drive folders (synced files and conflict backups)
// to the Drive trash, where Google keeps them for 30 days.
func (s *Service) TrashApp(ctx context.Context, appID string) (int, error) {
	c, err := s.client(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, kind := range []string{"apps", "backups"} {
		id, err := s.existingAppFolder(ctx, c, kind, appID)
		if err != nil {
			return n, err
		}
		if id == "" {
			continue
		}
		body, _ := json.Marshal(map[string]any{"trashed": true})
		req, _ := http.NewRequestWithContext(ctx, "PATCH", APIBase+"/files/"+url.PathEscape(id), bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if err := doJSON(c, req, nil); err != nil {
			return n, err
		}
		n++
		s.mu.Lock()
		for k, v := range s.children {
			if v == id {
				delete(s.children, k)
			}
		}
		s.mu.Unlock()
	}
	return n, nil
}
