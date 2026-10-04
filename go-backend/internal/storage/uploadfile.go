// Package storage -- pre-allocated (single file) chunked upload helpers.
//
// Upload layout (replaces the old "one directory + one file per chunk" model):
//
//   - INIT already knows fileSize (declared by the client, budgeted by the upload
//     quota) and chunkSize (server generated), so every chunk's [start, end)
//     byte range is known up-front. INIT therefore creates the FINAL storage file
//     once and extends it to fileSize (sparse on every mainstream filesystem), so
//     the upload never needs a second copy of the bytes.
//   - PUT writes its payload straight into that file's byte range with
//     pwrite(2) (os.File.WriteAt), which is atomic per call and idempotent: a
//     retried chunk simply rewrites the same range. No temp file, no rename.
//   - COMPLETE does no IO at all: the bytes are already in their final position,
//     so there is nothing to merge and no window where the API blocks on a large
//     file. The database is the single source of truth for "which chunks landed".
//
// All helpers are pure file IO: no DB access, no global locks, and every write
// targets a disjoint byte range, so parallel PUTs of the same upload cannot
// corrupt each other. A crash in the middle of a PUT can only leave a partially
// written range; because the receipt for that chunk is committed to the database
// only AFTER the bytes are durable, such a range is never considered received and
// the client simply re-uploads that chunk (rewriting the very same range).
package storage

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ErrChunkTooLarge is returned when a chunk body carries more bytes than its
// byte range allows. The overflow bytes are never written, so the following
// chunk's range stays intact.
var ErrChunkTooLarge = errors.New("chunk too large")

// FinalStoragePath mirrors StoreDataURL's naming: <timestamp><suffix> under dir.
// It returns the storage name and absolute destination without writing anything.
// The path is handed to a session at INIT and is already the FINAL clip path, so
// COMPLETE never moves the file.
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

// PreallocateFile creates path (if needed) and extends it to size bytes. The
// file is created with its final name and final length, which is what makes a
// merge-free COMPLETE possible: absence of bytes is represented by the file's
// size, presence of recorded bytes by the upload_chunks rows.
func PreallocateFile(path string, size int64) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("storage path is required")
	}
	if size < 0 {
		return errors.New("invalid file size")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("unable to prepare storage dir: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("unable to create upload file: %w", err)
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		return fmt.Errorf("unable to size upload file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("unable to flush upload file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("unable to close upload file: %w", err)
	}
	return nil
}

// EnsureFileSize guarantees that path exists with exactly size bytes. It reports
// recreated=true when the file had to be created or re-sized, which means any
// previously written byte range is gone and the caller must drop the database
// receipts (the DB, being the only source of truth for progress, would otherwise
// claim chunks that no longer exist).
func EnsureFileSize(path string, size int64) (recreated bool, err error) {
	if strings.TrimSpace(path) == "" {
		return false, errors.New("storage path is required")
	}
	info, statErr := os.Stat(path)
	if statErr == nil {
		if info.IsDir() {
			return false, fmt.Errorf("upload path %s is a directory", path)
		}
		if info.Size() == size {
			return false, nil
		}
	}
	if err := PreallocateFile(path, size); err != nil {
		return false, err
	}
	return true, nil
}

// ResetFile truncates path back to size bytes, discarding every written range.
// Used when the receipt table or the file itself was diagnosed as inconsistent.
func ResetFile(path string, size int64) error {
	return PreallocateFile(path, size)
}

// WriteChunkRange streams at most expected bytes from r into path at offset.
//
// Contract: it returns the exact number of bytes written. When the body is
// shorter than expected the caller compares written vs expected and rejects the
// chunk (leaving the partial range unrecorded, i.e. still missing). When the body
// is longer, the surplus byte is never written and ErrChunkTooLarge is returned.
func WriteChunkRange(path string, offset int64, expected int64, r io.Reader) (int64, error) {
	if offset < 0 {
		return 0, errors.New("invalid chunk offset")
	}
	if expected < 0 {
		return 0, errors.New("invalid chunk size")
	}
	if r == nil {
		return 0, errors.New("chunk reader is required")
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0o644)
	if err != nil {
		return 0, fmt.Errorf("unable to open upload file: %w", err)
	}
	defer f.Close()

	buf := make([]byte, 64<<10)
	var written int64
	for written < expected {
		want := int64(len(buf))
		if remaining := expected - written; remaining < want {
			want = remaining
		}
		read, readErr := io.ReadFull(r, buf[:want])
		if read > 0 {
			if _, writeErr := f.WriteAt(buf[:read], offset+written); writeErr != nil {
				return written, fmt.Errorf("unable to write chunk: %w", writeErr)
			}
			written += int64(read)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
				// Short body: report progress, let the caller reject the chunk.
				return written, nil
			}
			return written, fmt.Errorf("unable to read chunk: %w", readErr)
		}
	}
	// The whole range was filled: probe for a surplus byte without storing it.
	var probe [1]byte
	n, probeErr := r.Read(probe[:])
	if n > 0 {
		return written, ErrChunkTooLarge
	}
	if probeErr != nil && !errors.Is(probeErr, io.EOF) {
		return written, fmt.Errorf("unable to read chunk: %w", probeErr)
	}
	return written, nil
}

// FlushFile fsyncs the upload file so the recorded receipt never outlives the
// bytes it describes.
func FlushFile(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("unable to open upload file: %w", err)
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil {
		return fmt.Errorf("unable to flush upload file: %w", syncErr)
	}
	return closeErr
}

// VerifyFileSize reports the on-disk size of an upload file. It returns
// (size, false) without an error when the file does not exist, so callers can
// distinguish "lost file" (re-create + reset receipts) from "IO error".
func VerifyFileSize(path string) (int64, bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return info.Size(), true, nil
}

// RemoveUploadFile deletes an upload file (idempotent).
func RemoveUploadFile(path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// LegacyUploadRoot returns <baseDir>/uploads, the directory the previous
// per-chunk layout used (<baseDir>/uploads/<uploadId>/chunk-XXXXXX). Nothing in
// this version writes there; startup reconciliation removes it so an upgrade
// does not keep the old bytes (and their double disk usage) forever.
func LegacyUploadRoot(baseDir string) string {
	return filepath.Join(baseDir, "uploads")
}

// ListStorageFiles returns the names of the regular files directly under dir
// (sorted). It is used by startup reconciliation to find orphan upload files.
func ListStorageFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

// ChunkRange validates an index against chunkSize/fileSize and returns the
// [start, end) byte range that the chunk must fill. It is the single place where
// the offset math lives, so the API layer and the storage layer can never
// disagree about where a chunk belongs. Every chunk but the last one is required
// to be exactly chunkSize bytes.
func ChunkRange(index int, chunkSize int, fileSize int64) (offset int64, length int64, ok bool) {
	if chunkSize <= 0 || index < 0 || fileSize < 0 {
		return 0, 0, false
	}
	start := int64(index) * int64(chunkSize)
	if start >= fileSize {
		return 0, 0, false
	}
	end := start + int64(chunkSize)
	if end > fileSize {
		end = fileSize
	}
	return start, end - start, true
}
