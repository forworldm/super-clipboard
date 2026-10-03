// Package storage is the Go port of backend/storage.py.
package storage

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pixia1234/super-clipboard/backend/internal/apperr"
	"github.com/pixia1234/super-clipboard/backend/internal/models"
)

// extensionByMime reproduces the subset of Python's mimetypes.guess_extension
// that matters for browser uploads.
var extensionByMime = map[string]string{
	"application/json":            ".json",
	"application/octet-stream":    ".bin",
	"application/pdf":             ".pdf",
	"application/zip":             ".zip",
	"application/gzip":            ".gz",
	"application/x-tar":           ".tar",
	"application/x-7z-compressed": ".7z",
	"application/x-sh":            ".sh",
	"application/javascript":      ".js",
	"application/xml":             ".xml",
	"application/vnd.ms-excel":    ".xls",
	"application/msword":          ".doc",
	"image/png":                   ".png",
	"image/jpeg":                  ".jpg",
	"image/gif":                   ".gif",
	"image/webp":                  ".webp",
	"image/svg+xml":               ".svg",
	"image/bmp":                   ".bmp",
	"image/x-icon":                ".ico",
	"text/plain":                  ".txt",
	"text/html":                   ".html",
	"text/css":                    ".css",
	"text/csv":                    ".csv",
	"text/markdown":               ".md",
	"video/mp4":                   ".mp4",
	"video/webm":                  ".webm",
	"audio/mpeg":                  ".mp3",
	"audio/wav":                   ".wav",
	"audio/ogg":                   ".ogg",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   ".docx",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         ".xlsx",
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": ".pptx",
}

// GuessExtension mirrors `mimetypes.guess_extension(mime) or ""`.
func GuessExtension(mime string) string {
	if ext, ok := extensionByMime[strings.ToLower(strings.TrimSpace(mime))]; ok {
		return ext
	}
	return ""
}

// ParseDataURL mirrors `_parse_data_url`: it returns the declared mime type and
// the decoded payload, raising ValueError equivalents on malformed input.
func ParseDataURL(dataURL string) (string, []byte, error) {
	if !strings.HasPrefix(dataURL, "data:") {
		return "", nil, &apperr.ValueError{Message: "无效的文件数据 URL"}
	}
	idx := strings.Index(dataURL, ",")
	if idx < 0 {
		return "", nil, &apperr.ValueError{Message: "无效的文件数据 URL"}
	}
	header := dataURL[:idx]
	encoded := dataURL[idx+1:]

	mime := header
	if semicolon := strings.Index(mime, ";"); semicolon >= 0 {
		mime = mime[:semicolon]
	}
	mime = strings.TrimSpace(mime[len("data:"):])
	if mime == "" {
		mime = "application/octet-stream"
	}

	payload, err := decodeBase64Loose(encoded)
	if err != nil {
		return "", nil, &apperr.ValueError{Message: "文件数据解码失败"}
	}
	return mime, payload, nil
}

// decodeBase64Loose reproduces Python's `base64.b64decode` default behaviour:
// characters outside the alphabet are discarded before decoding and missing
// padding is tolerated.
func decodeBase64Loose(encoded string) ([]byte, error) {
	cleaned := make([]byte, 0, len(encoded))
	for i := 0; i < len(encoded); i++ {
		c := encoded[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '+' || c == '/' || c == '=' {
			cleaned = append(cleaned, c)
		}
	}
	if data, err := base64.StdEncoding.DecodeString(string(cleaned)); err == nil {
		return data, nil
	}
	if data, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(string(cleaned), "=")); err == nil {
		return data, nil
	}
	if data, err := base64.URLEncoding.DecodeString(string(cleaned)); err == nil {
		return data, nil
	}
	return nil, fmt.Errorf("invalid base64 payload")
}

// StoreDataURL mirrors `store_data_url`: it decodes the data URL, writes it into
// dir and returns the StoredFile metadata persisted alongside the clip.
func StoreDataURL(dir string, filename string, dataURL string) (*models.StoredFile, error) {
	mime, data, err := ParseDataURL(dataURL)
	if err != nil {
		return nil, err
	}

	safeName := filename
	if strings.TrimSpace(safeName) == "" {
		safeName = "uploaded"
	}

	now := time.Now().UTC()
	timestamp := fmt.Sprintf("%s%06d", now.Format("20060102150405"), now.Nanosecond()/1000)

	// Only the last path element contributes to the on-disk suffix so a crafted
	// filename cannot inject separators. The original name is kept as metadata.
	base := filepath.Base(strings.ReplaceAll(safeName, "\\", "/"))
	suffix := filepath.Ext(base)
	guessedSuffix := GuessExtension(mime)
	finalSuffix := suffix
	if finalSuffix == "" {
		finalSuffix = guessedSuffix
	}

	storageName := timestamp + finalSuffix
	destination := filepath.Join(dir, storageName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("unable to prepare storage dir: %w", err)
	}
	if err := os.WriteFile(destination, data, 0o644); err != nil {
		return nil, fmt.Errorf("unable to persist uploaded file: %w", err)
	}

	return &models.StoredFile{
		Name: safeName,
		Size: int64(len(data)),
		Mime: mime,
		Path: destination,
	}, nil
}
