// Package models is the Go port of backend/models.py.
package models

import "time"

// ClipType mirrors the `ClipType = str` alias.
type ClipType = string

// Known clip types (Literal["text", "file"] in schemas.py).
const (
	ClipTypeText ClipType = "text"
	ClipTypeFile ClipType = "file"
)

// StoredFile mirrors the `StoredFile` dataclass.
type StoredFile struct {
	Name string
	Size int64
	Mime string
	Path string
}

// Clip mirrors the `Clip` dataclass. Optional Python fields are pointers so
// that the JSON layer can emit explicit `null` values, exactly like pydantic.
type Clip struct {
	ID            string
	Type          ClipType
	CreatedAt     time.Time
	ExpiresAt     time.Time
	MaxDownloads  int
	DownloadCount int
	AccessCode    *string
	AccessToken   *string
	EnvironmentID string
	Text          *string
	StoredFile    *StoredFile
}

// IsExpired mirrors the `is_expired` property: `now(utc) >= expires_at`.
func (c *Clip) IsExpired() bool {
	return !time.Now().UTC().Before(c.ExpiresAt)
}

// ReachedDownloadLimit mirrors the `reached_download_limit` property.
func (c *Clip) ReachedDownloadLimit() bool {
	return c.DownloadCount >= c.MaxDownloads
}

// IsActive mirrors the `is_active` property.
func (c *Clip) IsActive() bool {
	return !c.IsExpired() && !c.ReachedDownloadLimit()
}

// TextValue returns the text payload or "" (Python: `clip.text or ""`).
func (c *Clip) TextValue() string {
	if c.Text == nil {
		return ""
	}
	return *c.Text
}

// AccessCodeValue returns the access code or "" (Python: `clip.access_code or ""`).
func (c *Clip) AccessCodeValue() string {
	if c.AccessCode == nil {
		return ""
	}
	return *c.AccessCode
}
