package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestStoredQuotaDefaults pins the shipped saved-clip budget. It must stay
// above the single-file cap so one allowed file always fits into an empty store
// (which is also why the default configuration raises no warning).
func TestStoredQuotaDefaults(t *testing.T) {
	dir := t.TempDir()
	settings, err := LoadFrom(testEnviron(dir), filepath.Join(dir, "missing.env"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if settings.StoredTotalQuotaBytes != DefaultStoredTotalQuotaBytes {
		t.Fatalf("stored quota default = %d, want %d", settings.StoredTotalQuotaBytes, DefaultStoredTotalQuotaBytes)
	}
	if DefaultStoredTotalQuotaBytes < settings.MaxFileSizeBytes {
		t.Fatalf("stored quota default (%d) must be >= MAX_FILE_SIZE_BYTES (%d)",
			DefaultStoredTotalQuotaBytes, settings.MaxFileSizeBytes)
	}
	if len(settings.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", settings.Warnings)
	}
}

// TestStoredQuotaEnvOverride covers the env variable, including 0 = unlimited.
func TestStoredQuotaEnvOverride(t *testing.T) {
	dir := t.TempDir()
	settings, err := LoadFrom(append(testEnviron(dir),
		"SUPER_CLIPBOARD_STORED_TOTAL_QUOTA_BYTES=4096"), filepath.Join(dir, "missing.env"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if settings.StoredTotalQuotaBytes != 4096 || settings.EffectiveStoredQuota() != 4096 {
		t.Fatalf("stored quota = %d/%d, want 4096",
			settings.StoredTotalQuotaBytes, settings.EffectiveStoredQuota())
	}

	dir = t.TempDir()
	unlimited, err := LoadFrom(append(testEnviron(dir),
		"SUPER_CLIPBOARD_STORED_TOTAL_QUOTA_BYTES=0"), filepath.Join(dir, "missing.env"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if unlimited.StoredTotalQuotaBytes != 0 || unlimited.EffectiveStoredQuota() != 0 {
		t.Fatalf("stored quota = %d, want 0 (unlimited)", unlimited.StoredTotalQuotaBytes)
	}
	// A hand-built Settings with a negative value is treated as "off" too.
	handmade := Defaults()
	handmade.StoredTotalQuotaBytes = -5
	if handmade.EffectiveStoredQuota() != 0 {
		t.Fatalf("negative stored quota must behave as unlimited, got %d", handmade.EffectiveStoredQuota())
	}
}

// TestStoredQuotaNegativeRejected: a negative budget is a configuration error.
func TestStoredQuotaNegativeRejected(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadFrom(append(testEnviron(dir),
		"SUPER_CLIPBOARD_STORED_TOTAL_QUOTA_BYTES=-1"), filepath.Join(dir, "missing.env"))
	if err == nil {
		t.Fatal("SUPER_CLIPBOARD_STORED_TOTAL_QUOTA_BYTES=-1 must be rejected")
	}
	if !strings.Contains(err.Error(), "must be >= 0") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestStoredQuotaBelowMaxFileSizeWarns: a store too small for a single allowed
// file warns at startup but must not stop the server.
func TestStoredQuotaBelowMaxFileSizeWarns(t *testing.T) {
	dir := t.TempDir()
	settings, err := LoadFrom(append(testEnviron(dir),
		"SUPER_CLIPBOARD_STORED_TOTAL_QUOTA_BYTES=1024",
		"SUPER_CLIPBOARD_MAX_FILE_SIZE_BYTES=2048",
		// Keep the upload budget comfortably above the file cap so exactly one
		// warning (the stored-clip one) is raised.
		"SUPER_CLIPBOARD_UPLOAD_TOTAL_QUOTA_BYTES=1048576",
	), filepath.Join(dir, "missing.env"))
	if err != nil {
		t.Fatalf("a small stored quota must not be fatal: %v", err)
	}
	if len(settings.Warnings) != 1 {
		t.Fatalf("expected exactly one warning, got %v", settings.Warnings)
	}
	if !strings.Contains(settings.Warnings[0], "STORED_TOTAL_QUOTA_BYTES") {
		t.Fatalf("warning must name the knob: %q", settings.Warnings[0])
	}

	// 0 disables the budget, so no warning is raised even below the file cap.
	dir = t.TempDir()
	unlimited, err := LoadFrom(append(testEnviron(dir),
		"SUPER_CLIPBOARD_STORED_TOTAL_QUOTA_BYTES=0",
		"SUPER_CLIPBOARD_MAX_FILE_SIZE_BYTES=2048",
		"SUPER_CLIPBOARD_UPLOAD_TOTAL_QUOTA_BYTES=1048576",
	), filepath.Join(dir, "missing.env"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(unlimited.Warnings) != 0 {
		t.Fatalf("unlimited stored quota must not warn: %v", unlimited.Warnings)
	}
}
