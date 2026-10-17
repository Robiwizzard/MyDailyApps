package kit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Drive API roots (overridable in tests).
var (
	DriveAPI    = "https://www.googleapis.com/drive/v3"
	DriveUpload = "https://www.googleapis.com/upload/drive/v3"
)

// DriveStorage stores files in one Drive folder, for example "Apps/cookbook".
// Client must be an authenticated *http.Client (see LoopbackLogin) with the
// drive.file scope; the folder path is created on first use.
type DriveStorage struct {
	Client *http.Client
	Folder string

	mu       sync.Mutex
	folderID string
}

// NewDriveStorage is a convenience constructor using Apps/<appID>.
func NewDriveStorage(client *http.Client, appID string) *DriveStorage {
	return &DriveStorage{Client: client, Folder: "Apps/" + appID}
}

type driveFile struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	ModifiedTime string `json:"modifiedTime"`
	Size         string `json:"size"`
}

func (d *DriveStorage) do(req *http.Request, out any) error {
	resp, err := d.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("drive: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func q(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `'`, `\'`) }

func (d *DriveStorage) query(ctx context.Context, query string) ([]driveFile, error) {
	var all []driveFile
	page := ""
	for {
		v := url.Values{"q": {query + " and trashed = false"}, "spaces": {"drive"}, "pageSize": {"200"},
			"fields": {"nextPageToken,files(id,name,modifiedTime,size)"}}
		if page != "" {
			v.Set("pageToken", page)
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", DriveAPI+"/files?"+v.Encode(), nil)
		var res struct {
			NextPageToken string      `json:"nextPageToken"`
			Files         []driveFile `json:"files"`
		}
		if err := d.do(req, &res); err != nil {
			return nil, err
		}
		all = append(all, res.Files...)
		if res.NextPageToken == "" {
			return all, nil
		}
		page = res.NextPageToken
	}
}

func (d *DriveStorage) folder(ctx context.Context) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.folderID != "" {
		return d.folderID, nil
	}
	parent := "root"
	for _, part := range strings.Split(strings.Trim(d.Folder, "/"), "/") {
		if part == "" {
			continue
		}
		found, err := d.query(ctx, fmt.Sprintf("name = '%s' and mimeType = 'application/vnd.google-apps.folder' and '%s' in parents", q(part), q(parent)))
		if err != nil {
			return "", err
		}
		if len(found) > 0 {
			parent = found[0].ID
			continue
		}
		body, _ := json.Marshal(map[string]any{"name": part, "mimeType": "application/vnd.google-apps.folder", "parents": []string{parent}})
		req, _ := http.NewRequestWithContext(ctx, "POST", DriveAPI+"/files?fields=id", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		var created driveFile
		if err := d.do(req, &created); err != nil {
			return "", err
		}
		parent = created.ID
	}
	d.folderID = parent
	return parent, nil
}

func (d *DriveStorage) find(ctx context.Context, name string) (*driveFile, error) {
	folder, err := d.folder(ctx)
	if err != nil {
		return nil, err
	}
	files, err := d.query(ctx, fmt.Sprintf("name = '%s' and '%s' in parents", q(name), q(folder)))
	if err != nil || len(files) == 0 {
		return nil, err
	}
	return &files[0], nil
}

func (d *DriveStorage) Put(ctx context.Context, name string, data []byte) error {
	f, err := d.find(ctx, name)
	if err != nil {
		return err
	}
	if f != nil {
		req, _ := http.NewRequestWithContext(ctx, "PATCH", DriveUpload+"/files/"+url.PathEscape(f.ID)+"?uploadType=media", bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/octet-stream")
		return d.do(req, nil)
	}
	folder, err := d.folder(ctx)
	if err != nil {
		return err
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	h := textproto.MIMEHeader{"Content-Type": {"application/json; charset=UTF-8"}}
	pw, _ := mw.CreatePart(h)
	meta, _ := json.Marshal(map[string]any{"name": name, "parents": []string{folder}})
	pw.Write(meta)
	pw, _ = mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/octet-stream"}})
	pw.Write(data)
	mw.Close()
	req, _ := http.NewRequestWithContext(ctx, "POST", DriveUpload+"/files?uploadType=multipart", &body)
	req.Header.Set("Content-Type", "multipart/related; boundary="+mw.Boundary())
	return d.do(req, nil)
}

func (d *DriveStorage) Get(ctx context.Context, name string) ([]byte, error) {
	f, err := d.find(ctx, name)
	if err != nil {
		return nil, err
	}
	if f == nil {
		return nil, ErrNotExist
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", DriveAPI+"/files/"+url.PathEscape(f.ID)+"?alt=media", nil)
	resp, err := d.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("drive: %s", resp.Status)
	}
	return io.ReadAll(resp.Body)
}

func (d *DriveStorage) List(ctx context.Context, prefix string) ([]Object, error) {
	folder, err := d.folder(ctx)
	if err != nil {
		return nil, err
	}
	query := fmt.Sprintf("'%s' in parents", q(folder))
	if prefix != "" {
		query += fmt.Sprintf(" and name contains '%s'", q(prefix)) // narrowed below
	}
	files, err := d.query(ctx, query)
	if err != nil {
		return nil, err
	}
	var out []Object
	for _, f := range files {
		if !strings.HasPrefix(f.Name, prefix) {
			continue
		}
		t, _ := time.Parse(time.RFC3339, f.ModifiedTime)
		var size int64
		fmt.Sscan(f.Size, &size)
		out = append(out, Object{Name: f.Name, Modified: t, Size: size})
	}
	sortNewest(out)
	return out, nil
}

func (d *DriveStorage) Delete(ctx context.Context, name string) error {
	f, err := d.find(ctx, name)
	if err != nil || f == nil {
		return err
	}
	req, _ := http.NewRequestWithContext(ctx, "DELETE", DriveAPI+"/files/"+url.PathEscape(f.ID), nil)
	return d.do(req, nil)
}
