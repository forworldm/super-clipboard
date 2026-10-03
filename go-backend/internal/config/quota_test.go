package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestUploadQuotaDefaults pins the documented defaults: 10 GiB of upload
// reservations, 1000 live sessions, a 1 GiB free-space watermark.
func TestUploadQuotaDefaults(t *testing.T) {
	dir := t.TempDir()
	settings, err := LoadFrom(testEnviron(dir), filepath.Join(dir, "missing.env"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if settings.UploadTotalQuotaBytes != DefaultUploadTotalQuotaBytes {
		t.Fatalf("quota default = %d, want %d", settings.UploadTotalQuotaBytes, DefaultUploadTotalQuotaBytes)
	}
	if settings.MaxActiveUploadSessions != DefaultMaxActiveUploadSessions {
		t.Fatalf("session default = %d, want %d", settings.MaxActiveUploadSessions, DefaultMaxActiveUploadSessions)
	}
	if settings.MinFreeDiskBytes != DefaultMinFreeDiskBytes {
		t.Fatalf("watermark default = %d, want %d", settings.MinFreeDiskBytes, DefaultMinFreeDiskBytes)
	}
	if len(settings.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", settings.Warnings)
	}
}

// TestUploadQuotaEnvOverrides covers the three env variables (0 = unlimited).
func TestUploadQuotaEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	environ := append(testEnviron(dir),
		"SUPER_CLIPBOARD_UPLOAD_TOTAL_QUOTA_BYTES=0",
		"SUPER_CLIPBOARD_MAX_ACTIVE_UPLOAD_SESSIONS=7",
		"SUPER_CLIPBOARD_MIN_FREE_DISK_BYTES=4096",
	)
	settings, err := LoadFrom(environ, filepath.Join(dir, "missing.env"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if settings.UploadTotalQuotaBytes != 0 || settings.EffectiveUploadQuota() != 0 {
		t.Fatalf("quota = %d, want 0 (unlimited)", settings.UploadTotalQuotaBytes)
	}
	if settings.MaxActiveUploadSessions != 7 || settings.EffectiveMaxActiveUploadSessions() != 7 {
		t.Fatalf("sessions = %d, want 7", settings.MaxActiveUploadSessions)
	}
	if settings.MinFreeDiskBytes != 4096 || settings.EffectiveMinFreeDiskBytes() != 4096 {
		t.Fatalf("watermark = %d, want 4096", settings.MinFreeDiskBytes)
	}
}

// TestUploadQuotaNegativeRejected: a negative limit is a configuration error.
func TestUploadQuotaNegativeRejected(t *testing.T) {
	cases := []string{
		"SUPER_CLIPBOARD_UPLOAD_TOTAL_QUOTA_BYTES=-1",
		"SUPER_CLIPBOARD_MAX_ACTIVE_UPLOAD_SESSIONS=-2",
		"SUPER_CLIPBOARD_MIN_FREE_DISK_BYTES=-3",
	}
	for _, entry := range cases {
		dir := t.TempDir()
		environ := append(testEnviron(dir), entry)
		if _, err := LoadFrom(environ, filepath.Join(dir, "missing.env")); err == nil {
			t.Fatalf("%s must be rejected", entry)
		} else if !strings.Contains(err.Error(), "must be >= 0") {
			t.Fatalf("%s: unexpected error %v", entry, err)
		}
	}
}

// TestUploadQuotaBelowMaxFileSizeWarns: a quota that cannot admit a full file
// warns at startup but must not stop the server.
func TestUploadQuotaBelowMaxFileSizeWarns(t *testing.T) {
	dir := t.TempDir()
	environ := append(testEnviron(dir),
		"SUPER_CLIPBOARD_UPLOAD_TOTAL_QUOTA_BYTES=1024",
		"SUPER_CLIPBOARD_MAX_FILE_SIZE_BYTES=2048",
	)
	settings, err := LoadFrom(environ, filepath.Join(dir, "missing.env"))
	if err != nil {
		t.Fatalf("a small quota must not be fatal: %v", err)
	}
	if len(settings.Warnings) != 1 {
		t.Fatalf("expected exactly one warning, got %v", settings.Warnings)
	}
	if !strings.Contains(settings.Warnings[0], "UPLOAD_TOTAL_QUOTA_BYTES") {
		t.Fatalf("warning must name the knob: %q", settings.Warnings[0])
	}

	// 0 disables the quota, so no warning is raised even below the file cap.
	dir = t.TempDir()
	environ = append(testEnviron(dir),
		"SUPER_CLIPBOARD_UPLOAD_TOTAL_QUOTA_BYTES=0",
		"SUPER_CLIPBOARD_MAX_FILE_SIZE_BYTES=2048",
	)
	unlimited, err := LoadFrom(environ, filepath.Join(dir, "missing.env"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(unlimited.Warnings) != 0 {
		t.Fatalf("unlimited quota must not warn: %v", unlimited.Warnings)
	}
}
