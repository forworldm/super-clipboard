package storage

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pixia1234/super-clipboard/backend/internal/apperr"
)

func assertValueError(t *testing.T, err error, expected string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %q, got nil", expected)
	}
	var valueError *apperr.ValueError
	if !isValueError(err, &valueError) {
		t.Fatalf("expected a ValueError, got %T (%v)", err, err)
	}
	if valueError.Message != expected {
		t.Fatalf("expected %q, got %q", expected, valueError.Message)
	}
}

func isValueError(err error, target **apperr.ValueError) bool {
	candidate, ok := err.(*apperr.ValueError)
	if !ok {
		return false
	}
	*target = candidate
	return true
}

// TestParseDataURL mirrors _parse_data_url.
func TestParseDataURL(t *testing.T) {
	payload := []byte("hello storage")
	encoded := base64.StdEncoding.EncodeToString(payload)

	mime, data, err := ParseDataURL("data:text/plain;base64," + encoded)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mime != "text/plain" {
		t.Fatalf("unexpected mime %q", mime)
	}
	if string(data) != string(payload) {
		t.Fatalf("unexpected payload %q", data)
	}

	mime, _, err = ParseDataURL("data:;base64," + encoded)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mime != "application/octet-stream" {
		t.Fatalf("expected the octet-stream fallback, got %q", mime)
	}

	mime, _, err = ParseDataURL("data:application/json;charset=utf-8;base64," + encoded)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mime != "application/json" {
		t.Fatalf("expected the mime before the first parameter, got %q", mime)
	}

	_, _, err = ParseDataURL("https://example.com/file.txt")
	assertValueError(t, err, "无效的文件数据 URL")

	_, _, err = ParseDataURL("data:text/plain;base64")
	assertValueError(t, err, "无效的文件数据 URL")

	// Python's base64.b64decode(validate=False) discards characters outside the
	// alphabet, so "###" decodes to an empty payload instead of failing.
	_, decoded, err := ParseDataURL("data:text/plain;base64,###")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(decoded) != 0 {
		t.Fatalf("expected an empty payload, got %q", decoded)
	}

	// A structurally broken payload still raises the ValueError equivalent.
	_, _, err = ParseDataURL("data:text/plain;base64,A")
	assertValueError(t, err, "文件数据解码失败")
}

// TestStoreDataURL mirrors store_data_url, including the file naming rules.
func TestStoreDataURL(t *testing.T) {
	dir := t.TempDir()
	payload := []byte("clip payload")
	dataURL := "data:text/plain;base64," + base64.StdEncoding.EncodeToString(payload)

	stored, err := StoreDataURL(dir, "notes.txt", dataURL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stored.Name != "notes.txt" || stored.Size != int64(len(payload)) || stored.Mime != "text/plain" {
		t.Fatalf("unexpected metadata %+v", stored)
	}
	if filepath.Dir(stored.Path) != dir {
		t.Fatalf("file should live in the storage dir: %s", stored.Path)
	}
	base := filepath.Base(stored.Path)
	if len(base) != 20+len(".txt") {
		t.Fatalf("unexpected storage name %q", base)
	}
	if !strings.HasSuffix(base, ".txt") {
		t.Fatalf("suffix should be preserved: %q", base)
	}
	content, err := os.ReadFile(stored.Path)
	if err != nil {
		t.Fatalf("unable to read stored file: %v", err)
	}
	if string(content) != string(payload) {
		t.Fatalf("unexpected content %q", content)
	}

	// A missing name falls back to "uploaded" and the mime is used for the suffix.
	anonymous, err := StoreDataURL(dir, "", "data:image/png;base64,"+base64.StdEncoding.EncodeToString([]byte{0x89, 0x50}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if anonymous.Name != "uploaded" {
		t.Fatalf("expected the uploaded fallback, got %q", anonymous.Name)
	}
	if !strings.HasSuffix(filepath.Base(anonymous.Path), ".png") {
		t.Fatalf("expected a guessed .png suffix, got %q", anonymous.Path)
	}

	// A broken payload raises the ValueError equivalent.
	if _, err := StoreDataURL(dir, "broken.txt", "not-a-data-url"); err == nil {
		t.Fatalf("expected an error")
	}
}

// TestGuessExtension covers the mimetypes.guess_extension subset.
func TestGuessExtension(t *testing.T) {
	cases := map[string]string{
		"text/plain":               ".txt",
		"image/png":                ".png",
		"application/octet-stream": ".bin",
		"application/unknown":      "",
	}
	for mime, expected := range cases {
		if got := GuessExtension(mime); got != expected {
			t.Fatalf("GuessExtension(%q) = %q, want %q", mime, got, expected)
		}
	}
}
