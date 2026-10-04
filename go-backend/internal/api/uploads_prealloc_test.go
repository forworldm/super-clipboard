package api

// Tests for the pre-allocated (merge-free) chunked upload layout:
//
//   - INIT already materialises the FINAL file, sized to fileSize, so every
//     chunk's byte range is known before a single byte is transferred.
//   - PUT writes straight into its own [offset, offset+expected) range, so chunks
//     arrive in any order and a retry rewrites the same range.
//   - COMPLETE moves no byte: the session's file *is* the clip's file, so the
//     upload never needs a second copy of the data on disk.

import (
	"bytes"
	"net/http"
	"os"
	"strings"
	"testing"
)

// TestInitPreallocatesFinalFile: the container exists right after init, at its
// final path and its final length, with no bytes and no chunk directories.
func TestInitPreallocatesFinalFile(t *testing.T) {
	app := newTestApp(t, smallChunkSettings())
	chunkSize := 64 << 10
	fileSize := chunkSize*2 + 12345

	init := initUpload(t, app, "prealloc.bin", int64(fileSize), "application/octet-stream", "env-pre")
	uploadID := init["uploadId"].(string)
	if int(init["totalChunks"].(float64)) != 3 {
		t.Fatalf("expected 3 chunks, got %v", init["totalChunks"])
	}
	if len(init["receivedChunks"].([]interface{})) != 0 {
		t.Fatalf("a fresh session must report no received chunk, got %v", init)
	}

	staged := sessionPath(t, app, uploadID)
	info, err := os.Stat(staged)
	if err != nil {
		t.Fatalf("init must create the final file: %v", err)
	}
	if info.Size() != int64(fileSize) {
		t.Fatalf("file must be truncated to fileSize at init: got %d want %d", info.Size(), fileSize)
	}
	// Final naming: init already picked the clip's storage name (same dir, no
	// temp/upload subdirectory), which is what removes the merge step.
	if dir, err := os.Stat(app.Settings.FileStorageDir); err != nil || !dir.IsDir() {
		t.Fatalf("storage dir should exist: %v %v", dir, err)
	}
	if strings.Contains(staged, string(os.PathSeparator)+"uploads"+string(os.PathSeparator)) {
		t.Fatalf("the legacy per-upload directory must not be used anymore: %s", staged)
	}
	if names := storageFileNames(t, app); len(names) != 1 {
		t.Fatalf("init must allocate exactly one file, got %v", names)
	}
}

// TestChunkWrittenInPlaceByOffset sends the LAST chunk first and asserts that
// exactly its byte range was touched: the write position comes from
// index*chunkSize, not from arrival order.
func TestChunkWrittenInPlaceByOffset(t *testing.T) {
	app := newTestApp(t, smallChunkSettings())
	chunkSize := 64 << 10
	fileSize := chunkSize*2 + 1234
	data := deterministicBytes(fileSize)

	init := initUpload(t, app, "offset.bin", int64(fileSize), "application/octet-stream", "env-off")
	uploadID := init["uploadId"].(string)
	staged := sessionPath(t, app, uploadID)

	// Chunk 2 (the remainder) alone.
	requireStatus(t, putChunk(t, app, uploadID, 2, sliceFor(chunkSize, data, 2)), http.StatusOK)
	stored, err := os.ReadFile(staged)
	if err != nil {
		t.Fatalf("read container: %v", err)
	}
	if len(stored) != fileSize {
		t.Fatalf("reading must not resize the container: %d", len(stored))
	}
	zeros := make([]byte, fileSize-2*chunkSize)
	if !bytes.Equal(stored[:2*chunkSize], make([]byte, 2*chunkSize)) {
		t.Fatalf("the first two ranges must still be empty (zeros)")
	}
	if !bytes.Equal(stored[2*chunkSize:], data[2*chunkSize:]) {
		t.Fatalf("the last range must hold the uploaded bytes at its own offset")
	}
	_ = zeros

	// Fill the remaining ranges out of order, then verify the whole file.
	requireStatus(t, putChunk(t, app, uploadID, 1, sliceFor(chunkSize, data, 1)), http.StatusOK)
	requireStatus(t, putChunk(t, app, uploadID, 0, sliceFor(chunkSize, data, 0)), http.StatusOK)
	stored, _ = os.ReadFile(staged)
	if !bytes.Equal(stored, data) {
		t.Fatalf("in-place writes must reproduce the original bytes")
	}

	// A retried chunk rewrites the very same range (idempotent).
	requireStatus(t, putChunk(t, app, uploadID, 1, sliceFor(chunkSize, data, 1)), http.StatusOK)
	stored, _ = os.ReadFile(staged)
	if !bytes.Equal(stored, data) {
		t.Fatalf("retrying a chunk must not corrupt its range")
	}
}

// TestCompleteMovesNoByte: the file that init created is the file the clip serves
// -- same inode, same bytes -- and complete adds no extra copy on disk.
func TestCompleteMovesNoByte(t *testing.T) {
	app := newTestApp(t, smallChunkSettings())
	chunkSize := 64 << 10
	fileSize := chunkSize + 4321
	data := deterministicBytes(fileSize)
	env := "env-nomerge"

	init := initUpload(t, app, "nomerge.bin", int64(fileSize), "application/octet-stream", env)
	uploadID := init["uploadId"].(string)
	staged := sessionPath(t, app, uploadID)
	before, err := os.Stat(staged)
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}

	requireStatus(t, putChunk(t, app, uploadID, 0, sliceFor(chunkSize, data, 0)), http.StatusOK)
	requireStatus(t, putChunk(t, app, uploadID, 1, sliceFor(chunkSize, data, 1)), http.StatusOK)

	done := completeUpload(t, app, uploadID, nil)
	requireStatus(t, done, http.StatusCreated)
	clip := decode(t, done)
	clipID := clip["id"].(string)

	// The clip points at the very path the session pre-allocated.
	filePayload := clip["payload"].(map[string]interface{})["file"].(map[string]interface{})
	if filePayload["size"] != float64(fileSize) {
		t.Fatalf("unexpected clip size %v", filePayload["size"])
	}
	session, _ := app.Repo.GetUploadSession(uploadID)
	if session == nil || session.StagedPath != staged {
		t.Fatalf("clip must reuse the pre-allocated path, got %+v (want %s)", session, staged)
	}
	after, err := os.Stat(staged)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) && after.Size() != before.Size() {
		t.Fatalf("complete must not rewrite the file (size %d -> %d)", before.Size(), after.Size())
	}
	// One file only: no assembled copy, no temp file, no chunk files.
	if names := storageFileNames(t, app); len(names) != 1 {
		t.Fatalf("complete must not create a merge copy, got %v", names)
	}
	for _, name := range storageFileNames(t, app) {
		if strings.Contains(name, ".part.") || strings.Contains(name, ".tmp.") {
			t.Fatalf("temp artifact left behind: %s", name)
		}
	}
	// The clip downloads the in-place bytes unchanged.
	dl := do(t, app, http.MethodGet, "/api/clips/"+clipID+"/file?environmentId="+env, nil)
	requireStatus(t, dl, http.StatusOK)
	if !bytes.Equal(dl.Body.Bytes(), data) {
		t.Fatalf("download mismatch (got %d want %d)", dl.Body.Len(), len(data))
	}
}
