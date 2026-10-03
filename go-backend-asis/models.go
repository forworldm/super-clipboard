package main

import (
	"time"
)

type StoredFile struct {
	Name string
	Size int64
	Mime string
	Path string
}

type Clip struct {
	ID             string
	Type           string
	CreatedAt      time.Time
	ExpiresAt      time.Time
	MaxDownloads   int
	DownloadCount  int
	AccessCode     string
	AccessToken    string
	EnvironmentID  string
	Text           string
	StoredFile     *StoredFile
}

func (c *Clip) IsExpired() bool {
	return time.Now().UTC().After(c.ExpiresAt) || time.Now().UTC().Equal(c.ExpiresAt)
}

func (c *Clip) ReachedDownloadLimit() bool {
	return c.DownloadCount >= c.MaxDownloads
}

func (c *Clip) IsActive() bool {
	return !c.IsExpired() && !c.ReachedDownloadLimit()
}
