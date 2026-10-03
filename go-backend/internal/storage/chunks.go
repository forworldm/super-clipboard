// Package storage -- chunked upload file helpers.
//
// All helpers are pure file IO with no DB access and no global locks, so chunk
// PUTs for different uploads (and different chunks of the same upload) run in
// parallel. Crash safety comes from atomic renames: chunk bytes are first
// written to <chunk>.tmp.<rand> then renamed over the final chunk path, so a
// power loss never leaves a half-written chunk behind under its real name.
package storage

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// UploadSessionDir returns <baseDir>/uploads/<uploadId>.
func UploadSessionDir(baseDir, uploadID string) string {
	return filepath.Join(baseDir, "uploads", uploadID)
}

// ChunkFilePath returns the durable path for one chunk.
func ChunkFilePath(baseDir, uploadID string, index int) string {
	return filepath.Join(UploadSessionDir(baseDir, uploadID), fmt.Sprintf("chunk-%06d", index))
}

// AssembledTempPath is the deterministic staging file used during COMPLETE.
// Assembly streams chunks here first; only after size verification is it
// renamed to the final clip file. On crash recovery this file is safe to
// delete because the source chunks are still intact.
func AssembledTempPath(baseDir, uploadID string) string {
	return filepath.Join(UploadSessionDir(baseDir, uploadID), "assembled.tmp")
}

// EnsureUploadDir creates the per-session directory.
func EnsureUploadDir(baseDir, uploadID string) error {
	if strings.TrimSpace(uploadID) == "" {
		return errors.New("upload id is required")
	}
	if strings.Contains(uploadID, "/") || strings.Contains(uploadID, "\\") || strings.Contains(uploadID, "..") {
		return errors.New("invalid upload id")
	}
	return os.MkdirAll(UploadSessionDir(baseDir, uploadID), 0o755)
}

// WriteChunkStream streams at most limit+1 bytes from r into the chunk file
// atomically (temp + rename). It returns the stored size. When the payload
// exceeds limit the temp file is removed and ErrChunkTooLarge is returned.
// Callers should set limit = expected chunk size (chunkSize for full chunks,
// remainder for the last one) so oversized POSTs are rejected without
// buffering the whole body in memory.
var ErrChunkTooLarge = errors.New("chunk too large")

// MissingChunkError reports which chunk index is missing during assembly.
type MissingChunkError struct {
	Index int
	Err   error
}

func (e *MissingChunkError) Error() string {
	if e == nil {
		return "missing chunk"
	}
	return fmt.Sprintf("missing chunk %d", e.Index)
}

func (e *MissingChunkError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func WriteChunkStream(baseDir, uploadID string, index int, r io.Reader, limit int64) (int64, error) {
	if index < 0 {
		return 0, errors.New("invalid chunk index")
	}
	if limit < 0 {
		return 0, errors.New("invalid chunk limit")
	}
	if err := EnsureUploadDir(baseDir, uploadID); err != nil {
		return 0, err
	}
	finalPath := ChunkFilePath(baseDir, uploadID, index)
	tmpPath := finalPath + ".tmp." + tempSuffix()
	out, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, fmt.Errorf("unable to create chunk temp file: %w", err)
	}
	// Copy at most limit+1 bytes to detect overflow without unbounded memory.
	written, copyErr := io.CopyN(out, r, limit+1)
	closeErr := out.Close()
	if copyErr != nil && !errors.Is(copyErr, io.EOF) {
		_ = os.Remove(tmpPath)
		return 0, fmt.Errorf("unable to write chunk: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return 0, fmt.Errorf("unable to write chunk: %w", closeErr)
	}
	if written > limit {
		_ = os.Remove(tmpPath)
		return written, ErrChunkTooLarge
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return 0, fmt.Errorf("unable to persist chunk: %w", err)
	}
	return written, nil
}

func tempSuffix() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

// AssembleChunks streams chunk files [0, totalChunks) in order into destPath
// without holding all bytes in memory. It verifies every chunk exists and that
// the assembled size equals expectedTotal. The destination is written via
// temp+rename for crash safety.
func AssembleChunks(baseDir, uploadID string, totalChunks int, destPath string, expectedTotal int64) error {
	if totalChunks < 0 || expectedTotal < 0 {
		return errors.New("invalid assembly range")
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("unable to prepare storage dir: %w", err)
	}
	tmpPath := destPath + ".part." + tempSuffix()
	out, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("unable to create assembled temp file: %w", err)
	}
	var assembled int64
	failed := true
	defer func() {
		_ = out.Close()
		if failed {
			_ = os.Remove(tmpPath)
		}
	}()
	buf := make([]byte, 32<<10)
	for i := 0; i < totalChunks; i++ {
		chunkPath := ChunkFilePath(baseDir, uploadID, i)
		in, err := os.Open(chunkPath)
		if err != nil {
			return &MissingChunkError{Index: i, Err: err}
		}
		n, err := io.CopyBuffer(out, in, buf)
		closeErr := in.Close()
		if err != nil {
			return fmt.Errorf("unable to assemble chunk %d: %w", i, err)
		}
		if closeErr != nil {
			return fmt.Errorf("unable to assemble chunk %d: %w", i, closeErr)
		}
		assembled += n
	}
	if assembled != expectedTotal {
		return fmt.Errorf("assembled size mismatch: got %d, want %d", assembled, expectedTotal)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("unable to finalize assembled file: %w", err)
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("unable to persist assembled file: %w", err)
	}
	failed = false
	return nil
}

// RemoveUploadDir deletes the per-session chunk directory (idempotent).
func RemoveUploadDir(baseDir, uploadID string) error {
	if strings.TrimSpace(uploadID) == "" {
		return nil
	}
	if strings.Contains(uploadID, "/") || strings.Contains(uploadID, "\\") || strings.Contains(uploadID, "..") {
		return errors.New("invalid upload id")
	}
	path := UploadSessionDir(baseDir, uploadID)
	if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ListChunkFilesOnDisk scans the session dir and returns index->size for every
// valid chunk-XXXXXX file present. Temp files (*.tmp.*) are ignored.
func ListChunkFilesOnDisk(baseDir, uploadID string) (map[int]int64, error) {
	dir := UploadSessionDir(baseDir, uploadID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[int]int64{}, nil
		}
		return nil, err
	}
	out := make(map[int]int64)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "chunk-") {
			continue
		}
		if strings.Contains(name, ".tmp.") {
			continue
		}
		rest := strings.TrimPrefix(name, "chunk-")
		idx, err := strconv.Atoi(rest)
		if err != nil || idx < 0 {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out[idx] = info.Size()
	}
	return out, nil
}

// ListUploadDirsOnDisk returns uploadIds that have a directory on disk.
func ListUploadDirsOnDisk(baseDir string) ([]string, error) {
	root := filepath.Join(baseDir, "uploads")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// FinalStoragePath mirrors StoreDataURL's naming: <timestamp><suffix> under dir.
// It returns the storage name and absolute destination without writing anything.
func FinalStoragePath(dir, filename, mime string) (string, string) {
	safeName := filename
	if strings.TrimSpace(safeName) == "" {
		safeName = "uploaded"
	}
	now := time.Now().UTC()
	timestamp := fmt.Sprintf("%s%06d", now.Format("20060102150405"), now.Nanosecond()/1000)
	base := filepath.Base(strings.ReplaceAll(safeName, "\\", "/"))
	suffix := filepath.Ext(base)
	if suffix == "" {
		suffix = GuessExtension(mime)
	}
	storageName := timestamp + suffix
	return storageName, filepath.Join(dir, storageName)
}
