package storage

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPreallocateAndWriteChunkRanges is the core of the new layout: one file,
// created at its final size by init, filled in place by disjoint byte ranges.
func TestPreallocateAndWriteChunkRanges(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "photo.png")
	chunkSize := 4096
	data := deterministic(3*chunkSize + 777)

	if err := PreallocateFile(dest, int64(len(data))); err != nil {
		t.Fatalf("preallocate: %v", err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != int64(len(data)) {
		t.Fatalf("file must be sized at init: got %d want %d", info.Size(), len(data))
	}

	// Write every range out of order, then re-write one (idempotent retry).
	order := []int{2, 0, 3, 1, 2}
	for _, index := range order {
		offset, length, ok := ChunkRange(index, chunkSize, int64(len(data)))
		if !ok {
			t.Fatalf("range %d should be valid", index)
		}
		payload := data[offset : offset+length]
		written, err := WriteChunkRange(dest, offset, length, bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("write %d: %v", index, err)
		}
		if written != length {
			t.Fatalf("write %d: got %d want %d", index, written, length)
		}
	}

	stored, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(stored, data) {
		t.Fatalf("in-place assembly mismatch (got %d want %d)", len(stored), len(data))
	}
}

// TestChunkRangeGeometry locks the size contract: every chunk but the last one is
// exactly chunkSize bytes, the last one carries the remainder.
func TestChunkRangeGeometry(t *testing.T) {
	cases := []struct {
		index              int
		chunkSize          int
		fileSize           int64
		wantOffset, wantOk int64
		wantLength         int64
	}{
		{index: 0, chunkSize: 1024, fileSize: 2500, wantOffset: 0, wantLength: 1024, wantOk: 1},
		{index: 1, chunkSize: 1024, fileSize: 2500, wantOffset: 1024, wantLength: 1024, wantOk: 1},
		{index: 2, chunkSize: 1024, fileSize: 2500, wantOffset: 2048, wantLength: 452, wantOk: 1},
		{index: 3, chunkSize: 1024, fileSize: 2500, wantOk: 0},
		{index: -1, chunkSize: 1024, fileSize: 2500, wantOk: 0},
		{index: 0, chunkSize: 0, fileSize: 2500, wantOk: 0},
		{index: 0, chunkSize: 1024, fileSize: 0, wantOk: 0},
	}
	for _, c := range cases {
		offset, length, ok := ChunkRange(c.index, c.chunkSize, c.fileSize)
		if c.wantOk == 1 {
			if !ok || offset != c.wantOffset || length != c.wantLength {
				t.Fatalf("range(%d,%d,%d) = %d,%d,%v want %d,%d,true",
					c.index, c.chunkSize, c.fileSize, offset, length, ok, c.wantOffset, c.wantLength)
			}
			continue
		}
		if ok {
			t.Fatalf("range(%d,%d,%d) should be invalid, got %d,%d", c.index, c.chunkSize, c.fileSize, offset, length)
		}
	}
}

// TestWriteChunkRangeRejectsOversize proves the overflow byte is never stored:
// the following chunk's range must stay intact.
func TestWriteChunkRangeRejectsOversize(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "over.bin")
	if err := PreallocateFile(dest, 8); err != nil {
		t.Fatalf("preallocate: %v", err)
	}
	// Fill both ranges honestly first.
	if _, err := WriteChunkRange(dest, 0, 4, bytes.NewReader([]byte("AAAA"))); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := WriteChunkRange(dest, 4, 4, bytes.NewReader([]byte("BBBB"))); err != nil {
		t.Fatalf("second: %v", err)
	}
	// A chunk that sends one byte too many is refused...
	written, err := WriteChunkRange(dest, 0, 4, bytes.NewReader([]byte("12345")))
	if !errors.Is(err, ErrChunkTooLarge) {
		t.Fatalf("expected ErrChunkTooLarge, got %v (written %d)", err, written)
	}
	if written != 4 {
		t.Fatalf("only the in-range bytes may be written, got %d", written)
	}
	// ...and the surplus byte must not have leaked into the next range.
	stored, _ := os.ReadFile(dest)
	if !bytes.Equal(stored, []byte("1234BBBB")) {
		t.Fatalf("unexpected file content %q", string(stored))
	}
}

// TestWriteChunkRangeShortBody: a truncated payload reports the short written
// count so the caller can refuse the chunk (no receipt is ever written).
func TestWriteChunkRangeShortBody(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "short.bin")
	if err := PreallocateFile(dest, 32); err != nil {
		t.Fatalf("preallocate: %v", err)
	}
	written, err := WriteChunkRange(dest, 16, 16, bytes.NewReader([]byte("abc")))
	if err != nil {
		t.Fatalf("short body should not be an IO error: %v", err)
	}
	if written != 3 {
		t.Fatalf("got %d want 3", written)
	}
}

func TestWriteChunkRangeValidation(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "v.bin")
	if err := PreallocateFile(dest, 16); err != nil {
		t.Fatalf("preallocate: %v", err)
	}
	if _, err := WriteChunkRange(dest, -1, 4, bytes.NewReader([]byte("a"))); err == nil {
		t.Fatalf("negative offset must fail")
	}
	if _, err := WriteChunkRange(dest, 0, -1, bytes.NewReader([]byte("a"))); err == nil {
		t.Fatalf("negative length must fail")
	}
	if _, err := WriteChunkRange(dest, 0, 4, nil); err == nil {
		t.Fatalf("missing reader must fail")
	}
	if _, err := WriteChunkRange(filepath.Join(dir, "missing.bin"), 0, 4, bytes.NewReader([]byte("abcd"))); err == nil {
		t.Fatalf("writing into a non-existent file must fail (init owns creation)")
	}
}

// TestPreallocateFileValidatesInput / traversal safety.
func TestPreallocateFileValidatesInput(t *testing.T) {
	dir := t.TempDir()
	if err := PreallocateFile("", 10); err == nil {
		t.Fatalf("empty path must fail")
	}
	if err := PreallocateFile(filepath.Join(dir, "x.bin"), -1); err == nil {
		t.Fatalf("negative size must fail")
	}
	// FinalStoragePath neutralises path traversal supplied through the filename.
	_, dest := FinalStoragePath(dir, "../../evil.txt", "text/plain")
	if strings.Contains(dest, "..") {
		t.Fatalf("traversal must be neutralised, got %s", dest)
	}
	if err := PreallocateFile(dest, 4); err != nil {
		t.Fatalf("preallocate: %v", err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("file should exist: %v", err)
	}
}

// TestEnsureFileSizeDetectsLoss covers the self-healing path: a container that
// vanished or changed size must be reported as recreated (the caller then drops
// the database receipts, because the byte ranges they describe are gone).
func TestEnsureFileSizeDetectsLoss(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "ensure.bin")

	recreated, err := EnsureFileSize(dest, 64)
	if err != nil {
		t.Fatalf("ensure (create): %v", err)
	}
	if !recreated {
		t.Fatalf("missing file must be reported as recreated")
	}
	recreated, err = EnsureFileSize(dest, 64)
	if err != nil {
		t.Fatalf("ensure (noop): %v", err)
	}
	if recreated {
		t.Fatalf("an intact file must not be reported as recreated")
	}
	recreated, err = EnsureFileSize(dest, 128)
	if err != nil {
		t.Fatalf("ensure (resize): %v", err)
	}
	if !recreated {
		t.Fatalf("a resized file must be reported as recreated")
	}
	if info, _ := os.Stat(dest); info.Size() != 128 {
		t.Fatalf("file should be resized, got %d", info.Size())
	}
	if err := RemoveUploadFile(dest); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if recreated, err = EnsureFileSize(dest, 64); err != nil || !recreated {
		t.Fatalf("after deletion: recreated=%v err=%v", recreated, err)
	}
	if err := RemoveUploadFile(dest); err != nil {
		t.Fatalf("remove again: %v", err)
	}
	if err := RemoveUploadFile(dest); err != nil {
		t.Fatalf("removing a missing file must be idempotent: %v", err)
	}
	if err := RemoveUploadFile(""); err != nil {
		t.Fatalf("empty path must be a no-op: %v", err)
	}
}

// TestListStorageFiles skips directories (the legacy layout is a directory) and
// returns sorted file names.
func TestListStorageFiles(t *testing.T) {
	dir := t.TempDir()
	if names, err := ListStorageFiles(dir); err != nil || len(names) != 0 {
		t.Fatalf("empty dir should list none: %v %v", names, err)
	}
	_ = os.WriteFile(filepath.Join(dir, "b.bin"), []byte("b"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "a.bin"), []byte("a"), 0o644)
	_ = os.MkdirAll(filepath.Join(dir, "uploads", "legacy"), 0o755)
	names, err := ListStorageFiles(dir)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(names) != 2 || names[0] != "a.bin" || names[1] != "b.bin" {
		t.Fatalf("unexpected names %v", names)
	}
	if _, err := ListStorageFiles(filepath.Join(dir, "nope")); err != nil {
		t.Fatalf("missing dir should be empty, not an error: %v", err)
	}
}

func TestVerifyFileSizeAndFlush(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "verify.bin")
	if err := PreallocateFile(dest, 10); err != nil {
		t.Fatalf("preallocate: %v", err)
	}
	size, ok, err := VerifyFileSize(dest)
	if err != nil || !ok || size != 10 {
		t.Fatalf("verify: %d %v %v", size, ok, err)
	}
	if err := FlushFile(dest); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if _, ok, err := VerifyFileSize(filepath.Join(dir, "missing.bin")); err != nil || ok {
		t.Fatalf("missing file must report ok=false without error: %v %v", ok, err)
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

func deterministic(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte((i*31 + 7) % 251)
	}
	return out
}
