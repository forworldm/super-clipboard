package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
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
	CaptchaProvider        string
	CaptchaSecret          string
	CaptchaTimeoutSeconds  float64
	CaptchaBypassToken     string
	CaptchaSiteKey         string
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv("SUPER_CLIPBOARD_" + key); ok {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v, ok := os.LookupEnv("SUPER_CLIPBOARD_" + key); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return fallback
}

func getEnvInt64(key string, fallback int64) int64 {
	if v, ok := os.LookupEnv("SUPER_CLIPBOARD_" + key); ok {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			return n
		}
	}
	return fallback
}

func getEnvFloat64(key string, fallback float64) float64 {
	if v, ok := os.LookupEnv("SUPER_CLIPBOARD_" + key); ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return f
		}
	}
	return fallback
}

func getEnvTrimmed(key, fallback string) string {
	v := strings.TrimSpace(getEnv(key, fallback))
	if v == "" {
		return ""
	}
	return v
}

func LoadConfig() *Config {
	cfg := &Config{
		DatabasePath:           getEnv("DATABASE_PATH", filepath.Join("backend", "storage", "clipboard.db")),
		FileStorageDir:         getEnv("FILE_STORAGE_DIR", filepath.Join("backend", "storage", "files")),
		AppHost:                getEnv("APP_HOST", "0.0.0.0"),
		AppPort:                getEnvInt("APP_PORT", 5173),
		DefaultMaxDownloads:    getEnvInt("DEFAULT_MAX_DOWNLOADS", 10),
		MaxAllowedDownloads:    getEnvInt("MAX_ALLOWED_DOWNLOADS", 500),
		CleanupIntervalSeconds: getEnvInt("CLEANUP_INTERVAL_SECONDS", 300),
		MaxFileSizeBytes:       getEnvInt64("MAX_FILE_SIZE_BYTES", 50*1024*1024),
		TokenExpiryHours:       getEnvInt("TOKEN_EXPIRY_HOURS", 720),
		StaticRoot:             getEnv("STATIC_ROOT", "dist"),
		CaptchaProvider:        strings.ToLower(strings.TrimSpace(getEnv("CAPTCHA_PROVIDER", ""))),
		CaptchaSecret:          getEnvTrimmed("CAPTCHA_SECRET", ""),
		CaptchaTimeoutSeconds:  getEnvFloat64("CAPTCHA_TIMEOUT_SECONDS", 6.0),
		CaptchaBypassToken:     getEnvTrimmed("CAPTCHA_BYPASS_TOKEN", ""),
		CaptchaSiteKey:         getEnvTrimmed("CAPTCHA_SITE_KEY", ""),
	}

	if cfg.CaptchaProvider != "" && cfg.CaptchaProvider != "turnstile" && cfg.CaptchaProvider != "recaptcha" {
		cfg.CaptchaProvider = ""
	}
	if cfg.CaptchaProvider == "" {
		cfg.CaptchaSecret = ""
	}

	// Ensure storage directories exist
	os.MkdirAll(cfg.FileStorageDir, 0o755)
	os.MkdirAll(filepath.Dir(cfg.DatabasePath), 0o755)

	return cfg
}
