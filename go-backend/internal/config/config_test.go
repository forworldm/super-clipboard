package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testEnviron(dir string) []string {
	return []string{
		"SUPER_CLIPBOARD_DATABASE_PATH=" + filepath.Join(dir, "db", "clipboard.db"),
		"SUPER_CLIPBOARD_FILE_STORAGE_DIR=" + filepath.Join(dir, "files"),
		"SUPER_CLIPBOARD_STATIC_ROOT=" + filepath.Join(dir, "dist"),
	}
}

// TestLoadDefaults mirrors the pydantic defaults of Settings.
func TestLoadDefaults(t *testing.T) {
	dir := t.TempDir()
	environ := append(testEnviron(dir),
		"PATH=/usr/bin",
		"UNRELATED=1",
	)
	settings, err := LoadFrom(environ, filepath.Join(dir, "missing.env"))
	if err != nil {
		t.Fatalf("unable to load settings: %v", err)
	}
	if settings.AppHost != "0.0.0.0" || settings.AppPort != 5173 {
		t.Fatalf("unexpected listen defaults: %s:%d", settings.AppHost, settings.AppPort)
	}
	if settings.DefaultMaxDownloads != 10 || settings.MaxAllowedDownloads != 500 {
		t.Fatalf("unexpected download defaults: %d/%d", settings.DefaultMaxDownloads, settings.MaxAllowedDownloads)
	}
	if settings.CleanupIntervalSeconds != 300 {
		t.Fatalf("unexpected cleanup interval: %d", settings.CleanupIntervalSeconds)
	}
	if settings.MaxFileSizeBytes != 50*1024*1024 {
		t.Fatalf("unexpected file size limit: %d", settings.MaxFileSizeBytes)
	}
	if settings.TokenExpiryHours != 720 {
		t.Fatalf("unexpected token expiry: %d", settings.TokenExpiryHours)
	}
	if settings.CaptchaTimeoutSeconds != 6.0 {
		t.Fatalf("unexpected captcha timeout: %v", settings.CaptchaTimeoutSeconds)
	}
	if settings.CaptchaEnabled() || settings.HasCaptchaSecret() {
		t.Fatalf("captcha should be disabled by default: %v", settings)
	}
	if info, err := os.Stat(settings.FileStorageDir); err != nil || !info.IsDir() {
		t.Fatalf("file storage dir should be created: %v", err)
	}
	if info, err := os.Stat(filepath.Dir(settings.DatabasePath)); err != nil || !info.IsDir() {
		t.Fatalf("database dir should be created: %v", err)
	}
}

// TestLoadOverrides covers the SUPER_CLIPBOARD_* environment variables.
func TestLoadOverrides(t *testing.T) {
	dir := t.TempDir()
	environ := append(testEnviron(dir),
		"SUPER_CLIPBOARD_APP_HOST=127.0.0.1",
		"SUPER_CLIPBOARD_APP_PORT=5174",
		"SUPER_CLIPBOARD_DEFAULT_MAX_DOWNLOADS=3",
		"SUPER_CLIPBOARD_MAX_ALLOWED_DOWNLOADS=42",
		"SUPER_CLIPBOARD_CLEANUP_INTERVAL_SECONDS=15",
		"SUPER_CLIPBOARD_MAX_FILE_SIZE_BYTES=2048",
		"SUPER_CLIPBOARD_TOKEN_EXPIRY_HOURS=24",
		"SUPER_CLIPBOARD_CAPTCHA_PROVIDER=  TurnStile  ",
		"SUPER_CLIPBOARD_CAPTCHA_SECRET=  s3cret  ",
		"SUPER_CLIPBOARD_CAPTCHA_BYPASS_TOKEN=  pass-me  ",
		"SUPER_CLIPBOARD_CAPTCHA_SITE_KEY=  site-key  ",
		"SUPER_CLIPBOARD_CAPTCHA_TIMEOUT_SECONDS=2.5",
	)
	settings, err := LoadFrom(environ, filepath.Join(dir, "missing.env"))
	if err != nil {
		t.Fatalf("unable to load settings: %v", err)
	}
	if settings.AppHost != "127.0.0.1" || settings.AppPort != 5174 {
		t.Fatalf("unexpected listen override: %s:%d", settings.AppHost, settings.AppPort)
	}
	if settings.DefaultMaxDownloads != 3 || settings.MaxAllowedDownloads != 42 {
		t.Fatalf("unexpected download override: %d/%d", settings.DefaultMaxDownloads, settings.MaxAllowedDownloads)
	}
	if settings.CleanupIntervalSeconds != 15 || settings.MaxFileSizeBytes != 2048 || settings.TokenExpiryHours != 24 {
		t.Fatalf("unexpected overrides: %+v", settings)
	}
	if settings.CaptchaProvider != "turnstile" {
		t.Fatalf("provider should be normalized, got %q", settings.CaptchaProvider)
	}
	if settings.CaptchaSecret != "s3cret" || settings.CaptchaBypassToken != "pass-me" || settings.CaptchaSiteKey != "site-key" {
		t.Fatalf("captcha values should be trimmed: %+v", settings)
	}
	if settings.CaptchaTimeoutSeconds != 2.5 {
		t.Fatalf("unexpected captcha timeout %v", settings.CaptchaTimeoutSeconds)
	}
	if !settings.CaptchaEnabled() {
		t.Fatalf("captcha should be enabled")
	}
}

// TestLoadRejectsUnknownProvider mirrors the normalize_provider validator.
func TestLoadRejectsUnknownProvider(t *testing.T) {
	dir := t.TempDir()
	environ := append(testEnviron(dir), "SUPER_CLIPBOARD_CAPTCHA_PROVIDER=hcaptcha")
	if _, err := LoadFrom(environ, filepath.Join(dir, "missing.env")); err == nil {
		t.Fatalf("expected a configuration error")
	} else if !strings.Contains(err.Error(), "captcha_provider must be turnstile or recaptcha") {
		t.Fatalf("unexpected error %v", err)
	}
}

// TestLoadRejectsInvalidNumbers mirrors pydantic's type validation.
func TestLoadRejectsInvalidNumbers(t *testing.T) {
	dir := t.TempDir()
	environ := append(testEnviron(dir), "SUPER_CLIPBOARD_APP_PORT=not-a-number")
	if _, err := LoadFrom(environ, filepath.Join(dir, "missing.env")); err == nil {
		t.Fatalf("expected a configuration error")
	}
}

// TestDotEnvFile covers the env_file fallback and its precedence rules.
func TestDotEnvFile(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	content := strings.Join([]string{
		"# comment line",
		"",
		"SUPER_CLIPBOARD_APP_PORT=6001",
		"SUPER_CLIPBOARD_CAPTCHA_PROVIDER=recaptcha",
		`SUPER_CLIPBOARD_CAPTCHA_SECRET="quoted secret"`,
		"SUPER_CLIPBOARD_APP_HOST=10.0.0.1",
	}, "\n")
	if err := os.WriteFile(envFile, []byte(content), 0o644); err != nil {
		t.Fatalf("unable to write .env: %v", err)
	}

	settings, err := LoadFrom(testEnviron(dir), envFile)
	if err != nil {
		t.Fatalf("unable to load settings: %v", err)
	}
	if settings.AppPort != 6001 {
		t.Fatalf("expected .env port, got %d", settings.AppPort)
	}
	if settings.CaptchaProvider != "recaptcha" {
		t.Fatalf("expected .env provider, got %q", settings.CaptchaProvider)
	}
	if settings.CaptchaSecret != "quoted secret" {
		t.Fatalf("expected unquoted secret, got %q", settings.CaptchaSecret)
	}

	// A real environment variable wins over the .env file.
	environ := append(testEnviron(dir), "SUPER_CLIPBOARD_APP_PORT=7002")
	settings, err = LoadFrom(environ, envFile)
	if err != nil {
		t.Fatalf("unable to load settings: %v", err)
	}
	if settings.AppPort != 7002 {
		t.Fatalf("environment should win over .env, got %d", settings.AppPort)
	}
}

// TestStaticHelpers covers the exists() checks used by main.py.
func TestStaticHelpers(t *testing.T) {
	dir := t.TempDir()
	settings := Defaults()
	settings.StaticRoot = filepath.Join(dir, "dist")
	settings.FileStorageDir = filepath.Join(dir, "files")

	if settings.StaticRootExists() || settings.AssetsDirExists() {
		t.Fatalf("static root should not exist yet")
	}
	if err := os.MkdirAll(filepath.Join(settings.StaticRoot, "assets"), 0o755); err != nil {
		t.Fatalf("unable to create assets dir: %v", err)
	}
	if !settings.StaticRootExists() || !settings.AssetsDirExists() {
		t.Fatalf("static root and assets should exist")
	}
	if settings.StaticIndex() != filepath.Join(settings.StaticRoot, "index.html") {
		t.Fatalf("unexpected index path %s", settings.StaticIndex())
	}
}
