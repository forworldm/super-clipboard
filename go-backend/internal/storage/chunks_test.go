package storage

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteChunkStreamAndAssemble(t *testing.T) {
	dir := t.TempDir()
	uploadID := "up-1"
	chunks := [][]byte{
		[]byte("hello "),
		[]byte("chunked "),
		[]byte("world"),
	}
	for i, c := range chunks {
		n, err := WriteChunkStream(dir, uploadID, i, bytes.NewReader(c), int64(len(c)))
		if err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if n != int64(len(c)) {
			t.Fatalf("write %d: got %d want %d", i, n, len(c))
		}
	}
	// Rewrite is idempotent.
	if _, err := WriteChunkStream(dir, uploadID, 1, bytes.NewReader(chunks[1]), int64(len(chunks[1]))); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	onDisk, err := ListChunkFilesOnDisk(dir, uploadID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(onDisk) != 3 {
		t.Fatalf("expected 3 chunks, got %v", onDisk)
	}
	dest := filepath.Join(dir, "final.bin")
	total := int64(len(chunks[0]) + len(chunks[1]) + len(chunks[2]))
	if err := AssembleChunks(dir, uploadID, 3, dest, total); err != nil {
		t.Fatalf("assemble: %v", err)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	if string(data) != "hello chunked world" {
		t.Fatalf("unexpected assembled %q", string(data))
	}
	// Size mismatch must fail without clobbering dest.
	if err := AssembleChunks(dir, uploadID, 3, dest, total+1); err == nil {
		t.Fatalf("size mismatch should fail")
	}
	// Missing chunk must fail.
	_ = os.Remove(ChunkFilePath(dir, uploadID, 1))
	if err := AssembleChunks(dir, uploadID, 3, filepath.Join(dir, "final2.bin"), total); err == nil {
		t.Fatalf("missing chunk should fail")
	} else if !strings.Contains(err.Error(), "missing chunk 1") {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestWriteChunkStreamTooLarge(t *testing.T) {
	dir := t.TempDir()
	_, err := WriteChunkStream(dir, "up-2", 0, bytes.NewReader([]byte("123456")), 3)
	if !errors.Is(err, ErrChunkTooLarge) {
		t.Fatalf("expected ErrChunkTooLarge, got %v", err)
	}
	if _, statErr := os.Stat(ChunkFilePath(dir, "up-2", 0)); !os.IsNotExist(statErr) {
		t.Fatalf("oversize chunk must not persist")
	}
	// Temp leftovers must not be listed as chunks.
	onDisk, err := ListChunkFilesOnDisk(dir, "up-2")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(onDisk) != 0 {
		t.Fatalf("expected no chunks, got %v", onDisk)
	}
}

func TestUploadDirValidation(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureUploadDir(dir, "../evil"); err == nil {
		t.Fatalf("path traversal should fail")
	}
	if err := RemoveUploadDir(dir, "../evil"); err == nil {
		t.Fatalf("path traversal remove should fail")
	}
	if err := RemoveUploadDir(dir, ""); err != nil {
		t.Fatalf("empty id remove should be no-op: %v", err)
	}
	if _, err := WriteChunkStream(dir, "ok", -1, bytes.NewReader([]byte("x")), 10); err == nil {
		t.Fatalf("negative index should fail")
	}
}

func TestListUploadDirsOnDisk(t *testing.T) {
	dir := t.TempDir()
	if dirs, err := ListUploadDirsOnDisk(dir); err != nil || len(dirs) != 0 {
		t.Fatalf("empty root should list none: %v %v", dirs, err)
	}
	_ = EnsureUploadDir(dir, "b-id")
	_ = EnsureUploadDir(dir, "a-id")
	dirs, err := ListUploadDirsOnDisk(dir)
	if err != nil {
		t.Fatalf("list dirs: %v", err)
	}
	if len(dirs) != 2 || dirs[0] != "a-id" || dirs[1] != "b-id" {
		t.Fatalf("unexpected dirs %v", dirs)
	}
	if err := RemoveUploadDir(dir, "a-id"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if dirs, _ := ListUploadDirsOnDisk(dir); len(dirs) != 1 {
		t.Fatalf("expected 1 dir left, got %v", dirs)
	}
}

func TestFinalStoragePath(t *testing.T) {
	_, dest := FinalStoragePath("/tmp/files", "photo", "image/png")
	if filepath.Ext(dest) != ".png" {
		t.Fatalf("mime suffix should apply, got %s", dest)
	}
	_, dest2 := FinalStoragePath("/tmp/files", "archive.tar.gz", "application/octet-stream")
	if filepath.Ext(dest2) != ".gz" {
		t.Fatalf("original suffix should win, got %s", dest2)
	}
	_, dest3 := FinalStoragePath("/tmp/files", "../../evil.txt", "text/plain")
	if strings.Contains(dest3, "..") {
		t.Fatalf("traversal must be neutralised, got %s", dest3)
	}
}
