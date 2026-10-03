package main

import (
	"encoding/base64"
	"errors"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// parseDataURL parses a data URL (data:[<mediatype>][;base64],<data>) and returns the mime type and decoded bytes.
func parseDataURL(dataURL string) (string, []byte, error) {
	if !strings.HasPrefix(dataURL, "data:") {
		return "", nil, errors.New("无效的文件数据 URL")
	}
	sep := strings.IndexByte(dataURL, ',')
	if sep < 0 {
		return "", nil, errors.New("无效的文件数据 URL")
	}
	header := dataURL[5:sep]
	encoded := dataURL[sep+1:]

	// mime type is everything before first ';'
	mediatype := header
	if i := strings.IndexByte(header, ';'); i >= 0 {
		mediatype = header[:i]
	}
	mediatype = strings.TrimSpace(mediatype)
	if mediatype == "" {
		mediatype = "application/octet-stream"
	}

	payload, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		// Some data URLs might use URLEncoding? Try that too
		payload, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(encoded, "="))
		if err != nil {
			return "", nil, errors.New("文件数据解码失败")
		}
	}
	return mediatype, payload, nil
}

// StoreDataURL decodes the data URL and writes the file into the configured storage directory.
func StoreDataURL(cfg *Config, filename, dataURL string) (*StoredFile, error) {
	mediatype, data, err := parseDataURL(dataURL)
	if err != nil {
		return nil, err
	}
	safeName := filename
	if safeName == "" {
		safeName = "uploaded"
	}
	timestamp := time.Now().UTC().Format("20060102150405.000000")
	timestamp = strings.ReplaceAll(timestamp, ".", "")
	suffix := filepath.Ext(safeName)
	guessedSuffix, _ := mime.ExtensionsByType(mediatype)
	guessedExt := ""
	if len(guessedSuffix) > 0 {
		guessedExt = guessedSuffix[0]
	}
	finalSuffix := suffix
	if finalSuffix == "" {
		finalSuffix = guessedExt
	}
	storageName := timestamp + finalSuffix
	destination := filepath.Join(cfg.FileStorageDir, storageName)
	if err := os.WriteFile(destination, data, 0o644); err != nil {
		return nil, err
	}
	return &StoredFile{
		Name: safeName,
		Size: int64(len(data)),
		Mime: mediatype,
		Path: destination,
	}, nil
}
