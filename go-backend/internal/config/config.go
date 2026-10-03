// Package config is the Go port of backend/config.py.
//
// Values are read from the process environment with the SUPER_CLIPBOARD_
// prefix and, as a fallback, from a local .env file (pydantic-settings
// behaviour: real environment variables always win over the .env file).
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// EnvPrefix mirrors `model_config = SettingsConfigDict(env_prefix="SUPER_CLIPBOARD_")`.
const EnvPrefix = "SUPER_CLIPBOARD_"

// Settings mirrors the pydantic `Settings` model.
//
// Optional Python strings (`str | None`) are represented by Go strings where the
// empty value means `None`; helper predicates such as CaptchaEnabled make the
// intent explicit at the call sites.
type Settings struct {
	DatabasePath           string
	FileStorageDir         string
	AppHost                string
	AppPort                int
	DefaultMaxDownloads    int
	MaxAllowedDownloads    int
	CleanupIntervalSeconds int
	MaxFileSizeBytes       int64
	TokenExpiryHours       int
	StaticRoot             string
	CaptchaProvider        string // "" | "turnstile" | "recaptcha"
	CaptchaSecret          string
	CaptchaTimeoutSeconds  float64
	CaptchaBypassToken     string
	CaptchaSiteKey         string
	// Admin API auth key (empty means disabled).
	AdminAPIKey string
	// Chunked upload settings (server-generated, never trust client values).
	UploadChunkSizeBytes    int
	UploadSessionTTLSeconds int
	// Global upload disk quota (0 = unlimited). These three guards keep chunked
	// uploads from eating the whole host disk: a per-session byte budget, a cap
	// on concurrent sessions and a free-space watermark.
	UploadTotalQuotaBytes   int64 // total bytes reserved by live upload sessions
	MaxActiveUploadSessions int   // max concurrently active upload sessions
	MinFreeDiskBytes        int64 // free-space watermark below which writes stop
	// Warnings collects non-fatal configuration findings (surfaced at startup).
	Warnings []string
}

// Global upload quota defaults: 10 GiB of reservations, 1000 live sessions and
// a 1 GiB free-space watermark. Zero disables the matching guard.
const (
	DefaultUploadTotalQuotaBytes   int64 = 10 << 30 // 10 GiB
	DefaultMaxActiveUploadSessions       = 1000
	DefaultMinFreeDiskBytes        int64 = 1 << 30 // 1 GiB
)

// Defaults returns the same defaults as the Python Settings class.
func Defaults() *Settings {
	return &Settings{
		DatabasePath:            "backend/storage/clipboard.db",
		FileStorageDir:          "backend/storage/files",
		AppHost:                 "0.0.0.0",
		AppPort:                 5173,
		DefaultMaxDownloads:     10,
		MaxAllowedDownloads:     500,
		CleanupIntervalSeconds:  300,
		MaxFileSizeBytes:        50 * 1024 * 1024,
		TokenExpiryHours:        720,
		StaticRoot:              "dist",
		CaptchaTimeoutSeconds:   6.0,
		UploadChunkSizeBytes:    1 << 20,      // 1 MiB per chunk, server-generated
		UploadSessionTTLSeconds: 24 * 60 * 60, // 24h resume window
		UploadTotalQuotaBytes:   DefaultUploadTotalQuotaBytes,
		MaxActiveUploadSessions: DefaultMaxActiveUploadSessions,
		MinFreeDiskBytes:        DefaultMinFreeDiskBytes,
	}
}

// MinUploadChunkSizeBytes / MaxUploadChunkSizeBytes bound the server-generated
// chunk size so a misconfigured env cannot produce absurd sessions.
const (
	MinUploadChunkSizeBytes = 64 << 10      // 64 KiB
	MaxUploadChunkSizeBytes = 8 << 20       // 8 MiB
	MinUploadTTLSeconds     = 60            // 1 minute (tests use small TTLs)
	MaxUploadTTLSeconds     = 7 * 24 * 3600 // 7 days
)

// EffectiveChunkSize returns a sane chunk size even if Settings was built
// manually in tests without going through Defaults()/Load().
func (s *Settings) EffectiveChunkSize() int {
	if s == nil || s.UploadChunkSizeBytes <= 0 {
		return 1 << 20
	}
	if s.UploadChunkSizeBytes < MinUploadChunkSizeBytes {
		return MinUploadChunkSizeBytes
	}
	if s.UploadChunkSizeBytes > MaxUploadChunkSizeBytes {
		return MaxUploadChunkSizeBytes
	}
	return s.UploadChunkSizeBytes
}

// EffectiveUploadTTL returns a sane session TTL in seconds.
func (s *Settings) EffectiveUploadTTL() int {
	if s == nil || s.UploadSessionTTLSeconds <= 0 {
		return 24 * 60 * 60
	}
	if s.UploadSessionTTLSeconds < MinUploadTTLSeconds {
		return MinUploadTTLSeconds
	}
	if s.UploadSessionTTLSeconds > MaxUploadTTLSeconds {
		return MaxUploadTTLSeconds
	}
	return s.UploadSessionTTLSeconds
}

// EffectiveUploadQuota returns the global upload byte budget (0 = unlimited).
// Hand-built Settings in tests may carry negatives; treat them as "off".
func (s *Settings) EffectiveUploadQuota() int64 {
	if s == nil || s.UploadTotalQuotaBytes <= 0 {
		return 0
	}
	return s.UploadTotalQuotaBytes
}

// EffectiveMaxActiveUploadSessions returns the live-session cap (0 = unlimited).
func (s *Settings) EffectiveMaxActiveUploadSessions() int {
	if s == nil || s.MaxActiveUploadSessions <= 0 {
		return 0
	}
	return s.MaxActiveUploadSessions
}

// EffectiveMinFreeDiskBytes returns the free-space watermark (0 = unlimited).
func (s *Settings) EffectiveMinFreeDiskBytes() int64 {
	if s == nil || s.MinFreeDiskBytes <= 0 {
		return 0
	}
	return s.MinFreeDiskBytes
}

// CaptchaEnabled reports whether a captcha provider is configured.
func (s *Settings) CaptchaEnabled() bool { return s.CaptchaProvider != "" }

// HasCaptchaSecret reports whether the captcha secret is present.
func (s *Settings) HasCaptchaSecret() bool { return s.CaptchaSecret != "" }

// StaticRootExists mirrors `if settings.static_root.exists()` in main.py.
func (s *Settings) StaticRootExists() bool {
	info, err := os.Stat(s.StaticRoot)
	return err == nil && info.IsDir()
}

// AssetsDir returns the "<static_root>/assets" directory.
func (s *Settings) AssetsDir() string { return filepath.Join(s.StaticRoot, "assets") }

// AssetsDirExists mirrors `assets_dir.exists()`.
func (s *Settings) AssetsDirExists() bool {
	info, err := os.Stat(s.AssetsDir())
	return err == nil && info.IsDir()
}

// StaticIndex returns "<static_root>/index.html".
func (s *Settings) StaticIndex() string { return filepath.Join(s.StaticRoot, "index.html") }

// Load reads settings from the environment and the .env file in the working
// directory, then creates the storage directories (same side effects as the
// Python module import).
func Load() (*Settings, error) {
	return LoadFrom(os.Environ(), ".env")
}

// LoadFrom is the testable core of Load.
func LoadFrom(environ []string, envFilePath string) (*Settings, error) {
	osEnv := make(map[string]string, len(environ))
	for _, entry := range environ {
		if idx := strings.IndexByte(entry, '='); idx >= 0 {
			osEnv[entry[:idx]] = entry[idx+1:]
		}
	}
	fileEnv := ParseDotEnvFile(envFilePath)

	lookup := func(name string) (string, bool) {
		key := EnvPrefix + name
		if value, ok := osEnv[key]; ok && strings.TrimSpace(value) != "" {
			return value, true
		}
		if value, ok := fileEnv[key]; ok && strings.TrimSpace(value) != "" {
			return value, true
		}
		return "", false
	}

	s := Defaults()

	str := func(name string, target *string) {
		if value, ok := lookup(name); ok {
			*target = value
		}
	}
	str("DATABASE_PATH", &s.DatabasePath)
	str("FILE_STORAGE_DIR", &s.FileStorageDir)
	str("APP_HOST", &s.AppHost)
	str("STATIC_ROOT", &s.StaticRoot)
	str("ADMIN_API_KEY", &s.AdminAPIKey)

	intVar := func(name string, target *int) error {
		value, ok := lookup(name)
		if !ok {
			return nil
		}
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("%s%s: input should be a valid integer, unable to parse string as an integer", EnvPrefix, name)
		}
		*target = parsed
		return nil
	}
	int64Var := func(name string, target *int64) error {
		value, ok := lookup(name)
		if !ok {
			return nil
		}
		parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return fmt.Errorf("%s%s: input should be a valid integer, unable to parse string as an integer", EnvPrefix, name)
		}
		*target = parsed
		return nil
	}
	floatVar := func(name string, target *float64) error {
		value, ok := lookup(name)
		if !ok {
			return nil
		}
		parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			return fmt.Errorf("%s%s: input should be a valid number, unable to parse string as a number", EnvPrefix, name)
		}
		*target = parsed
		return nil
	}

	steps := []func() error{
		func() error { return intVar("APP_PORT", &s.AppPort) },
		func() error { return intVar("DEFAULT_MAX_DOWNLOADS", &s.DefaultMaxDownloads) },
		func() error { return intVar("MAX_ALLOWED_DOWNLOADS", &s.MaxAllowedDownloads) },
		func() error { return intVar("CLEANUP_INTERVAL_SECONDS", &s.CleanupIntervalSeconds) },
		func() error { return int64Var("MAX_FILE_SIZE_BYTES", &s.MaxFileSizeBytes) },
		func() error { return intVar("TOKEN_EXPIRY_HOURS", &s.TokenExpiryHours) },
		func() error { return floatVar("CAPTCHA_TIMEOUT_SECONDS", &s.CaptchaTimeoutSeconds) },
		func() error { return intVar("UPLOAD_CHUNK_SIZE_BYTES", &s.UploadChunkSizeBytes) },
		func() error { return intVar("UPLOAD_SESSION_TTL_SECONDS", &s.UploadSessionTTLSeconds) },
		func() error { return int64Var("UPLOAD_TOTAL_QUOTA_BYTES", &s.UploadTotalQuotaBytes) },
		func() error { return intVar("MAX_ACTIVE_UPLOAD_SESSIONS", &s.MaxActiveUploadSessions) },
		func() error { return int64Var("MIN_FREE_DISK_BYTES", &s.MinFreeDiskBytes) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return nil, err
		}
	}
	// Global upload quota guards: negatives are configuration errors (a
	// negative budget is meaningless), 0 means "unlimited".
	negativeGuard := []struct {
		name  string
		value int64
	}{
		{"UPLOAD_TOTAL_QUOTA_BYTES", s.UploadTotalQuotaBytes},
		{"MAX_ACTIVE_UPLOAD_SESSIONS", int64(s.MaxActiveUploadSessions)},
		{"MIN_FREE_DISK_BYTES", s.MinFreeDiskBytes},
	}
	for _, guard := range negativeGuard {
		if guard.value < 0 {
			return nil, fmt.Errorf("%s%s: value error, must be >= 0 (0 disables the limit)", EnvPrefix, guard.name)
		}
	}
	// A total quota below the single-file cap can never admit a big file. This
	// is a deployment smell, not a hard error: small files still work, so warn
	// and keep serving.
	if s.UploadTotalQuotaBytes > 0 && s.MaxFileSizeBytes > 0 && s.UploadTotalQuotaBytes < s.MaxFileSizeBytes {
		s.Warnings = append(s.Warnings, fmt.Sprintf(
			"%sUPLOAD_TOTAL_QUOTA_BYTES (%d bytes) < %sMAX_FILE_SIZE_BYTES (%d bytes): 接近上限的单文件将无法开始上传",
			EnvPrefix, s.UploadTotalQuotaBytes, EnvPrefix, s.MaxFileSizeBytes))
	}

	// Clamp chunked-upload knobs so a bad env cannot break the protocol.
	// Effective*() also guards hand-built Settings in tests.
	s.UploadChunkSizeBytes = s.EffectiveChunkSize()
	s.UploadSessionTTLSeconds = s.EffectiveUploadTTL()

	// @field_validator("captcha_provider") normalize_provider
	if value, ok := lookup("CAPTCHA_PROVIDER"); ok {
		normalized := strings.ToLower(strings.TrimSpace(value))
		if normalized != "turnstile" && normalized != "recaptcha" {
			return nil, fmt.Errorf("%sCAPTCHA_PROVIDER: value error, captcha_provider must be turnstile or recaptcha", EnvPrefix)
		}
		s.CaptchaProvider = normalized
	}

	// @field_validator("captcha_secret", "captcha_bypass_token", "captcha_site_key") trim_optional
	trimmed := func(name string) string {
		value, ok := lookup(name)
		if !ok {
			return ""
		}
		return strings.TrimSpace(value)
	}
	s.CaptchaSecret = trimmed("CAPTCHA_SECRET")
	s.CaptchaBypassToken = trimmed("CAPTCHA_BYPASS_TOKEN")
	s.CaptchaSiteKey = trimmed("CAPTCHA_SITE_KEY")
	s.AdminAPIKey = strings.TrimSpace(s.AdminAPIKey)

	// Module import side effects: mkdir(parents=True, exist_ok=True)
	if err := os.MkdirAll(s.FileStorageDir, 0o755); err != nil {
		return nil, fmt.Errorf("unable to create file storage dir: %w", err)
	}
	if parent := filepath.Dir(s.DatabasePath); parent != "" && parent != "." {
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return nil, fmt.Errorf("unable to create database dir: %w", err)
		}
	}
	return s, nil
}
