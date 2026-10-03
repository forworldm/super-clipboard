package models

import (
	"testing"
	"time"
)

func TestClipActivity(t *testing.T) {
	now := time.Now().UTC()
	clip := &Clip{
		ExpiresAt:     now.Add(time.Hour),
		MaxDownloads:  2,
		DownloadCount: 0,
	}
	if !clip.IsActive() || clip.IsExpired() || clip.ReachedDownloadLimit() {
		t.Fatalf("fresh clip should be active")
	}

	clip.ExpiresAt = now.Add(-time.Second)
	if !clip.IsExpired() || clip.IsActive() {
		t.Fatalf("past expiry should be inactive")
	}

	clip.ExpiresAt = now.Add(time.Hour)
	clip.DownloadCount = 2
	if !clip.ReachedDownloadLimit() || clip.IsActive() {
		t.Fatalf("download limit should deactivate the clip")
	}
}

func TestClipHelpers(t *testing.T) {
	clip := &Clip{}
	if clip.TextValue() != "" || clip.AccessCodeValue() != "" {
		t.Fatalf("nil optional fields should yield empty strings")
	}
	text := "hello"
	code := "12345"
	clip.Text = &text
	clip.AccessCode = &code
	if clip.TextValue() != "hello" || clip.AccessCodeValue() != "12345" {
		t.Fatalf("unexpected helper values %q %q", clip.TextValue(), clip.AccessCodeValue())
	}
}
