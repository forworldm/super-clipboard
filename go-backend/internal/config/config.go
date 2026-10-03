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
}

// Defaults returns the same defaults as the Python Settings class.
func Defaults() *Settings {
	return &Settings{
		DatabasePath:           "backend/storage/clipboard.db",
		FileStorageDir:         "backend/storage/files",
		AppHost:                "0.0.0.0",
		AppPort:                5173,
		DefaultMaxDownloads:    10,
		MaxAllowedDownloads:    500,
		CleanupIntervalSeconds: 300,
		MaxFileSizeBytes:       50 * 1024 * 1024,
		TokenExpiryHours:       720,
		StaticRoot:             "dist",
		CaptchaTimeoutSeconds:  6.0,
	}
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
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return nil, err
		}
	}

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
