package host

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const pictureFile = "profile-picture.txt"

// Settings is the GET /api/settings payload.
type Settings struct {
	ProfilePicture string    `json:"profilePicture,omitempty"` // data URL
	SyncPicture    bool      `json:"syncPicture"`
	Drive          DriveInfo `json:"drive"`
	Mode           string    `json:"mode"`
	DataDir        string    `json:"dataDir"`
	AppsDir        string    `json:"appsDir"`
}

type DriveInfo struct {
	Configured bool   `json:"configured"`
	Connected  bool   `json:"connected"`
	Email      string `json:"email,omitempty"`
}

func (h *Host) Settings() (Settings, error) {
	pic, _ := h.Store.Setting("profile_picture")
	sync, _ := h.Store.Setting("sync_picture")
	return Settings{
		ProfilePicture: pic, SyncPicture: sync == "true",
		Drive: DriveInfo{Configured: h.Drive.Configured(), Connected: h.Drive.Connected(), Email: h.Drive.Email()},
		Mode:  string(h.Cfg.Mode), DataDir: h.Cfg.DataDir, AppsDir: h.Cfg.AppsDir,
	}, nil
}

// SettingsUpdate is the PUT /api/settings body. Fields left out are unchanged.
type SettingsUpdate struct {
	ProfilePicture *string `json:"profilePicture"` // "" removes it
	SyncPicture    *bool   `json:"syncPicture"`
}

func validPicture(p string) error {
	if p == "" {
		return nil
	}
	if !strings.HasPrefix(p, "data:image/") || !strings.Contains(p[:min(len(p), 64)], ";base64,") {
		return errors.New("profile picture must be an image")
	}
	if len(p) > 3<<20 {
		return errors.New("profile picture is too large (max about 2 MB)")
	}
	return nil
}

func (h *Host) UpdateSettings(ctx context.Context, u SettingsUpdate) error {
	if u.SyncPicture != nil {
		v := ""
		if *u.SyncPicture {
			v = "true"
		}
		if err := h.Store.SetSetting("sync_picture", v); err != nil {
			return err
		}
	}
	if u.ProfilePicture != nil {
		if err := validPicture(*u.ProfilePicture); err != nil {
			return err
		}
		if err := h.Store.SetSetting("profile_picture", *u.ProfilePicture); err != nil {
			return err
		}
	}
	// Optional: keep the picture in Drive next to the library file.
	sync, _ := h.Store.Setting("sync_picture")
	if sync == "true" && h.Drive.Connected() && (u.ProfilePicture != nil || u.SyncPicture != nil) {
		pic, _ := h.Store.Setting("profile_picture")
		if pic != "" {
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if err := h.Drive.UploadFile(ctx, pictureFile, "text/plain", []byte(pic)); err != nil {
				return fmt.Errorf("saved locally, but the Drive copy failed: %w", err)
			}
		}
	}
	h.bump()
	return nil
}
