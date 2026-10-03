// Package repository -- chunked upload sessions.
//
// Design notes (read before modifying):
//
//   - Sessions are persisted in SQLite (upload_sessions + upload_chunks) so a
//     power failure / process restart does not lose resume state. Chunk bytes
//     live on disk under <FileStorageDir>/uploads/<uploadId>/chunk-XXXXXX.
//   - Concurrency: the global r.mu serialises short DB transactions only. File
//     IO (chunk writes, assembly, deletions) ALWAYS happens outside r.mu so a
//     slow disk or a 50MB assembly never blocks unrelated uploads/clips.
//     Cross-operation races (PUT vs COMPLETE vs DELETE) are resolved with
//     atomic SQL predicates (e.g. UPDATE ... WHERE status='active') plus
//     atomic file renames; see handlers for the protocol.
//   - Crash recovery: COMPLETE flips active->completing (recording the staged
//     path) before any assembly IO. If the server dies mid-assembly the row
//     stays in `completing` and startup reconciliation (ResetStuckCompleting
//   - orphan sweeps) rolls it back to `active`.
package repository

import (
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Upload statuses persisted in upload_sessions.status.
const (
	UploadStatusActive     = "active"
	UploadStatusCompleting = "completing"
	UploadStatusCompleted  = "completed"
)

var (
	ErrUploadAlreadyCompleted = errors.New("upload already completed")
	ErrChunkIndexOutOfRange   = errors.New("chunk index out of range")
)

// UploadSession mirrors one row of upload_sessions.
type UploadSession struct {
	ID            string
	Filename      string
	FileSize      int64
	MimeType      string
	ChunkSize     int
	TotalChunks   int
	Status        string
	EnvironmentID string
	RequestID     string // client idempotency key (empty for legacy sessions)
	CreatedAt     int64  // unix seconds
	UpdatedAt     int64  // unix seconds
	ExpiresAt     int64  // unix seconds
	StagedPath    string // final assembled file (set when completing/completed)
	ClipID        string // clip created by complete (empty for file-only sessions)
	// QuotaReleased reports whether this session already returned its upload
	// quota reservation. The flag + a CAS UPDATE makes release idempotent:
	// abort/expire/complete can race, only one of them decrements the ledger.
	QuotaReleased bool
}

// IsExpired reports whether the session passed its TTL.
func (s *UploadSession) IsExpired(nowUnix int64) bool {
	if s == nil {
		return true
	}
	return nowUnix >= s.ExpiresAt
}

// ExpectedChunkSize returns the exact byte size a chunk index must carry.
// The last chunk carries the remainder; empty files have 0 chunks.
func (s *UploadSession) ExpectedChunkSize(index int) (int64, bool) {
	if s == nil || index < 0 || index >= s.TotalChunks {
		return 0, false
	}
	if s.TotalChunks == 0 {
		return 0, false
	}
	if index < s.TotalChunks-1 {
		return int64(s.ChunkSize), true
	}
	// Last chunk: remainder (may equal ChunkSize when evenly divisible).
	consumed := int64(s.TotalChunks-1) * int64(s.ChunkSize)
	remainder := s.FileSize - consumed
	if remainder < 0 {
		remainder = 0
	}
	return remainder, true
}

// MissingChunks computes sorted missing indices from a received list.
func MissingChunks(total int, received []int) []int {
	if total <= 0 {
		return nil
	}
	seen := make(map[int]bool, len(received))
	for _, v := range received {
		seen[v] = true
	}
	var missing []int
	for i := 0; i < total; i++ {
		if !seen[i] {
			missing = append(missing, i)
		}
	}
	return missing
}

// CalcTotalChunks derives the server-side chunk count. uploadId/chunkSize are
// always server-generated; the client-declared fileSize only feeds this math.
func CalcTotalChunks(fileSize int64, chunkSize int) int {
	if fileSize <= 0 || chunkSize <= 0 {
		return 0
	}
	return int((fileSize + int64(chunkSize) - 1) / int64(chunkSize))
}

func scanUploadSession(scan func(dest ...interface{}) error) (*UploadSession, error) {
	var (
		id, filename, mime, status, env string
		fileSize                        int64
		chunkSize, totalChunks          int
		createdAt, updatedAt, expiresAt int64
		stagedPath, clipID, requestID   sql.NullString
		quotaReleased                   int
	)
	if err := scan(&id, &filename, &fileSize, &mime, &chunkSize, &totalChunks,
		&status, &env, &createdAt, &updatedAt, &expiresAt, &stagedPath, &clipID, &requestID,
		&quotaReleased); err != nil {
		return nil, err
	}
	return &UploadSession{
		ID: id, Filename: filename, FileSize: fileSize, MimeType: mime,
		ChunkSize: chunkSize, TotalChunks: totalChunks, Status: status,
		EnvironmentID: env, RequestID: requestID.String,
		CreatedAt: createdAt, UpdatedAt: updatedAt,
		ExpiresAt: expiresAt, StagedPath: stagedPath.String, ClipID: clipID.String,
		QuotaReleased: quotaReleased != 0,
	}, nil
}

const uploadColumns = `id, filename, file_size, mime_type, chunk_size, total_chunks,
	status, environment_id, created_at, updated_at, expires_at, staged_path, clip_id, request_id,
	quota_released`

// CreateUploadSessionParams groups arguments for CreateUploadSession.
type CreateUploadSessionParams struct {
	Filename      string
	FileSize      int64
	MimeType      string
	EnvironmentID string
	RequestID     string // optional client idempotency key
	ChunkSize     int
	TTLSeconds    int
}

// normalizeUploadParams applies defaults and validates an init request.
func (r *ClipRepository) normalizeUploadParams(params CreateUploadSessionParams) (*UploadSession, error) {
	filename := strings.TrimSpace(params.Filename)
	if filename == "" {
		return nil, errors.New("filename is required")
	}
	if params.FileSize < 0 {
		return nil, errors.New("file size must be >= 0")
	}
	chunkSize := params.ChunkSize
	if chunkSize <= 0 {
		chunkSize = r.settings.EffectiveChunkSize()
	}
	ttl := params.TTLSeconds
	if ttl <= 0 {
		ttl = r.settings.EffectiveUploadTTL()
	}
	total := CalcTotalChunks(params.FileSize, chunkSize)
	mime := strings.TrimSpace(params.MimeType)
	if mime == "" {
		mime = "application/octet-stream"
	}
	now := nowUnix()
	return &UploadSession{
		ID: uuid.NewString(), Filename: filename, FileSize: params.FileSize,
		MimeType: mime, ChunkSize: chunkSize, TotalChunks: total,
		Status: UploadStatusActive, EnvironmentID: strings.TrimSpace(params.EnvironmentID),
		RequestID: strings.TrimSpace(params.RequestID),
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now + int64(ttl),
	}, nil
}

// CreateUploadSession inserts a new active session. Pure DB work (short lock).
func (r *ClipRepository) CreateUploadSession(params CreateUploadSessionParams) (*UploadSession, error) {
	session, err := r.normalizeUploadParams(params)
	if err != nil {
		return nil, err
	}
	if err := r.insertUploadSession(session); err != nil {
		return nil, err
	}
	return session, nil
}

// insertUploadSession performs the raw INSERT (short lock).
func (r *ClipRepository) insertUploadSession(session *UploadSession) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.db.Exec(`INSERT INTO upload_sessions (`+uploadColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?, 0)`,
		session.ID, session.Filename, session.FileSize, session.MimeType,
		session.ChunkSize, session.TotalChunks, session.Status,
		session.EnvironmentID, session.CreatedAt, session.UpdatedAt, session.ExpiresAt,
		nullIfEmpty(session.RequestID),
	)
	return err
}

// GetUploadSessionByRequestID looks up the idempotency key (env, requestID).
// Returns (nil, nil) when no session carries that key.
func (r *ClipRepository) GetUploadSessionByRequestID(environmentID string, requestID string) (*UploadSession, error) {
	if strings.TrimSpace(requestID) == "" {
		return nil, nil
	}
	session, err := scanUploadSession(r.db.QueryRow(
		"SELECT "+uploadColumns+" FROM upload_sessions WHERE environment_id = ? AND request_id = ?",
		strings.TrimSpace(environmentID), strings.TrimSpace(requestID)).Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return session, nil
}

// CreateOrGetUploadSession implements idempotent init on (env, requestID):
//   - a live session for the key is returned with created=false (replay);
//   - an expired session for the key is purged and replaced with a fresh one;
//   - without a requestID this always creates (no dedup possible).
//
// Two parallel inits race on the partial UNIQUE index: the loser re-reads the
// winner's row and returns it, so exactly one session consumes the key.
func (r *ClipRepository) CreateOrGetUploadSession(params CreateUploadSessionParams) (session *UploadSession, created bool, err error) {
	requestID := strings.TrimSpace(params.RequestID)
	env := strings.TrimSpace(params.EnvironmentID)
	for attempt := 0; attempt < 3; attempt++ {
		if requestID != "" {
			existing, lookupErr := r.GetUploadSessionByRequestID(env, requestID)
			if lookupErr != nil {
				return nil, false, lookupErr
			}
			if existing != nil {
				if !existing.IsExpired(nowUnix()) {
					return existing, false, nil
				}
				// Expired key: purge the stale row so the fresh insert below can
				// reuse the index slot. File cleanup happens outside the repo.
				if _, abortErr := r.AbortUploadSession(existing.ID); abortErr != nil &&
					!errors.Is(abortErr, sql.ErrNoRows) {
					return nil, false, abortErr
				}
			}
		}
		fresh, normErr := r.normalizeUploadParams(params)
		if normErr != nil {
			return nil, false, normErr
		}
		insertErr := r.insertUploadSession(fresh)
		if insertErr == nil {
			return fresh, true, nil
		}
		if requestID != "" && isUniqueConstraintError(insertErr) {
			// Lost the insert race: read the winner.
			existing, lookupErr := r.GetUploadSessionByRequestID(env, requestID)
			if lookupErr == nil && existing != nil {
				return existing, false, nil
			}
			continue // transient: retry the whole cycle
		}
		return nil, false, insertErr
	}
	// Should be unreachable for bounded retries; treat as replay of the winner.
	existing, lookupErr := r.GetUploadSessionByRequestID(env, requestID)
	if lookupErr == nil && existing != nil {
		return existing, false, nil
	}
	return nil, false, errors.New("unable to create upload session (idempotency conflict)")
}

// isUniqueConstraintError matches any SQLite UNIQUE violation (table columns
// or partial index), covering both modernc and cgo drivers' messages.
func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// GetUploadSession loads a session by id (lock-free read). Returns (nil, nil)
// when the id does not exist.
func (r *ClipRepository) GetUploadSession(uploadID string) (*UploadSession, error) {
	session, err := scanUploadSession(r.db.QueryRow(
		"SELECT "+uploadColumns+" FROM upload_sessions WHERE id = ?", uploadID).Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return session, nil
}

// ListReceivedChunks returns sorted received indices (lock-free read).
func (r *ClipRepository) ListReceivedChunks(uploadID string) ([]int, error) {
	rows, err := r.db.Query(
		"SELECT chunk_index FROM upload_chunks WHERE upload_id = ? ORDER BY chunk_index ASC", uploadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var idx int
		if err := rows.Scan(&idx); err != nil {
			return nil, err
		}
		out = append(out, idx)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if out == nil {
		out = []int{}
	}
	return out, nil
}

// GetUploadSessionWithChunks loads session + received indices.
func (r *ClipRepository) GetUploadSessionWithChunks(uploadID string) (*UploadSession, []int, error) {
	session, err := r.GetUploadSession(uploadID)
	if err != nil || session == nil {
		return session, nil, err
	}
	received, err := r.ListReceivedChunks(uploadID)
	if err != nil {
		return nil, nil, err
	}
	return session, received, nil
}

// MarkChunkReceived upserts a chunk receipt and bumps updated_at.
// It validates the session is still active/completing-eligible; callers must
// have already durably written the chunk file (IO outside the lock).
func (r *ClipRepository) MarkChunkReceived(uploadID string, index int, size int64) error {
	if strings.TrimSpace(uploadID) == "" || index < 0 || size < 0 {
		return errors.New("invalid chunk receipt")
	}
	now := nowUnix()
	r.mu.Lock()
	defer r.mu.Unlock()
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	var status string
	var total int
	if err := tx.QueryRow(
		"SELECT status, total_chunks FROM upload_sessions WHERE id = ?", uploadID).Scan(&status, &total); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sql.ErrNoRows
		}
		return err
	}
	if status == UploadStatusCompleted {
		return ErrUploadAlreadyCompleted
	}
	if index >= total {
		return ErrChunkIndexOutOfRange
	}
	if _, err := tx.Exec(`INSERT INTO upload_chunks (upload_id, chunk_index, size, received_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(upload_id, chunk_index) DO UPDATE SET size=excluded.size, received_at=excluded.received_at`,
		uploadID, index, size, now); err != nil {
		return err
	}
	if _, err := tx.Exec(
		"UPDATE upload_sessions SET updated_at = ? WHERE id = ?", now, uploadID); err != nil {
		return err
	}
	return tx.Commit()
}

// MissingChunksError is returned when COMPLETE finds gaps.
type MissingChunksError struct {
	Missing []int
}

func (e *MissingChunksError) Error() string { return "chunks missing" }

// TryBeginComplete atomically transitions active->completing (recording the
// staged path for crash recovery) after verifying every chunk is present.
// On success the caller owns assembly IO outside the lock; on failure the
// session stays active for retry/resume.
func (r *ClipRepository) TryBeginComplete(uploadID string, stagedPath string) (*UploadSession, []int, error) {
	now := nowUnix()
	r.mu.Lock()
	defer r.mu.Unlock()
	tx, err := r.db.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback() //nolint:errcheck
	session, err := scanUploadSession(tx.QueryRow(
		"SELECT "+uploadColumns+" FROM upload_sessions WHERE id = ?", uploadID).Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, sql.ErrNoRows
		}
		return nil, nil, err
	}
	if session.ExpiresAt <= now {
		return nil, nil, &SessionExpiredError{Session: session}
	}
	switch session.Status {
	case UploadStatusCompleted:
		return nil, nil, &SessionCompletedError{Session: session}
	case UploadStatusCompleting:
		return nil, nil, &SessionCompletingError{Session: session}
	case UploadStatusActive:
	default:
		return nil, nil, errors.New("unknown upload status: " + session.Status)
	}
	rows, err := tx.Query(
		"SELECT chunk_index FROM upload_chunks WHERE upload_id = ? ORDER BY chunk_index ASC", uploadID)
	if err != nil {
		return nil, nil, err
	}
	var received []int
	for rows.Next() {
		var idx int
		if err := rows.Scan(&idx); err != nil {
			rows.Close()
			return nil, nil, err
		}
		received = append(received, idx)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if missing := MissingChunks(session.TotalChunks, received); len(missing) > 0 {
		return nil, nil, &MissingChunksError{Missing: missing}
	}
	res, err := tx.Exec(`UPDATE upload_sessions SET status = ?, staged_path = ?, updated_at = ?
		WHERE id = ? AND status = ?`,
		UploadStatusCompleting, nullIfEmpty(stagedPath), now, uploadID, UploadStatusActive)
	if err != nil {
		return nil, nil, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, nil, err
	}
	if affected != 1 {
		return nil, nil, &SessionCompletingError{Session: session}
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	session.Status = UploadStatusCompleting
	session.StagedPath = stagedPath
	session.UpdatedAt = now
	sort.Ints(received)
	return session, received, nil
}

// SessionExpiredError / SessionCompletedError / SessionCompletingError carry
// the loaded session so handlers can render precise statuses.
type SessionExpiredError struct{ Session *UploadSession }

func (e *SessionExpiredError) Error() string { return "upload session expired" }

type SessionCompletedError struct{ Session *UploadSession }

func (e *SessionCompletedError) Error() string { return "upload already completed" }

type SessionCompletingError struct{ Session *UploadSession }

func (e *SessionCompletingError) Error() string { return "upload is completing" }

func nullIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// CompleteUploadSession flips completing->completed (recording clip linkage).
func (r *ClipRepository) CompleteUploadSession(uploadID string, clipID string) (*UploadSession, error) {
	now := nowUnix()
	r.mu.Lock()
	defer r.mu.Unlock()
	tx, err := r.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck
	var fileSize int64
	if err := tx.QueryRow(
		"SELECT file_size FROM upload_sessions WHERE id = ?", uploadID).Scan(&fileSize); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("upload session not in completing state (aborted or expired?)")
		}
		return nil, err
	}
	res, err := tx.Exec(`UPDATE upload_sessions SET status = ?, clip_id = ?, updated_at = ?
		WHERE id = ? AND status = ?`,
		UploadStatusCompleted, nullIfEmpty(clipID), now, uploadID, UploadStatusCompleting)
	if err != nil {
		return nil, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected != 1 {
		return nil, errors.New("upload session not in completing state (aborted or expired?)")
	}
	// Option A: a finished upload returns its reservation in the very same
	// transaction, so a crash can never leave the bytes reserved forever.
	if _, err := r.releaseQuotaTx(tx, uploadID, fileSize); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	session, err := scanUploadSession(r.db.QueryRow(
		"SELECT "+uploadColumns+" FROM upload_sessions WHERE id = ?", uploadID).Scan)
	if err != nil {
		return nil, err
	}
	return session, nil
}

// FailComplete rolls completing->active (clearing staged_path) so a failed
// assembly can be retried. Returns the previous staged path for file cleanup.
func (r *ClipRepository) FailComplete(uploadID string) (string, error) {
	now := nowUnix()
	r.mu.Lock()
	defer r.mu.Unlock()
	tx, err := r.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback() //nolint:errcheck
	var (
		staged   sql.NullString
		fileSize int64
	)
	if err := tx.QueryRow(
		"SELECT staged_path, file_size FROM upload_sessions WHERE id = ?", uploadID).Scan(&staged, &fileSize); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil // already aborted: nothing to roll back
		}
		return "", err
	}
	if _, err := tx.Exec(`UPDATE upload_sessions SET status = ?, staged_path = NULL, updated_at = ?
		WHERE id = ? AND status = ?`,
		UploadStatusActive, now, uploadID, UploadStatusCompleting); err != nil {
		return "", err
	}
	// complete 失败回滚同样归还预留，且只能归还一次（quota_released CAS）。
	if _, err := r.releaseQuotaTx(tx, uploadID, fileSize); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return staged.String, nil
}

// AbortUploadSession deletes the session + chunk rows atomically and returns
// the session for file cleanup outside the lock. Completed sessions linked to
// a clip are NOT abortable (delete the clip instead); a session mid-assembly
// (`completing`) is also refused so a cancel can never tear down an in-flight
// merge and leave its staged file orphaned — the merge rolls back on crash and
// the caller can retry DELETE after that.
func (r *ClipRepository) AbortUploadSession(uploadID string) (*UploadSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tx, err := r.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck
	session, err := scanUploadSession(tx.QueryRow(
		"SELECT "+uploadColumns+" FROM upload_sessions WHERE id = ?", uploadID).Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	if session.Status == UploadStatusCompleting {
		return nil, &SessionCompletingError{Session: session}
	}
	if session.Status == UploadStatusCompleted && session.ClipID != "" {
		return nil, &SessionCompletedError{Session: session}
	}
	// Return the reservation before the row disappears; the CAS keeps this
	// idempotent when abort races expiry cleanup or a complete rollback.
	if _, err := r.releaseQuotaTx(tx, session.ID, session.FileSize); err != nil {
		return nil, err
	}
	if _, err := tx.Exec("DELETE FROM upload_chunks WHERE upload_id = ?", uploadID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec("DELETE FROM upload_sessions WHERE id = ?", uploadID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return session, nil
}

// UploadPurgeVictim describes files to delete outside the lock after a DB purge.
type UploadPurgeVictim struct {
	UploadID   string
	StagedPath string
	HasClip    bool
	FileSize   int64
}

// PurgeExpiredUploads deletes expired sessions (any status) and returns victims
// for file cleanup outside the lock. Completed sessions with a clip keep their
// clip file; only the session row + chunk dir are dropped.
func (r *ClipRepository) PurgeExpiredUploads(nowUnixSec int64) ([]UploadPurgeVictim, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rows, err := r.db.Query(
		"SELECT id, staged_path, clip_id, file_size FROM upload_sessions WHERE expires_at <= ?", nowUnixSec)
	if err != nil {
		return nil, err
	}
	var victims []UploadPurgeVictim
	for rows.Next() {
		var id string
		var fileSize int64
		var staged, clip sql.NullString
		if err := rows.Scan(&id, &staged, &clip, &fileSize); err != nil {
			rows.Close()
			return nil, err
		}
		victims = append(victims, UploadPurgeVictim{
			UploadID: id, StagedPath: staged.String, HasClip: clip.String != "",
			FileSize: fileSize,
		})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(victims) == 0 {
		return nil, nil
	}
	tx, err := r.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck
	for _, victim := range victims {
		// Expiry cleanup returns the reservation exactly once, before the row
		// (which carries the quota_released flag) is deleted.
		if _, err := r.releaseQuotaTx(tx, victim.UploadID, victim.FileSize); err != nil {
			return nil, err
		}
		if _, err := tx.Exec("DELETE FROM upload_chunks WHERE upload_id = ?", victim.UploadID); err != nil {
			return nil, err
		}
		if _, err := tx.Exec("DELETE FROM upload_sessions WHERE id = ?", victim.UploadID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return victims, nil
}

// StuckCompleting describes completing sessions found at startup (power loss).
type StuckCompleting struct {
	UploadID   string
	StagedPath string
}

// ResetStuckCompleting rolls every `completing` row back to `active` (clearing
// staged_path) and returns them for temp-file cleanup outside the lock.
func (r *ClipRepository) ResetStuckCompleting() ([]StuckCompleting, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rows, err := r.db.Query(
		"SELECT id, staged_path FROM upload_sessions WHERE status = ?", UploadStatusCompleting)
	if err != nil {
		return nil, err
	}
	var stuck []StuckCompleting
	for rows.Next() {
		var id string
		var staged sql.NullString
		if err := rows.Scan(&id, &staged); err != nil {
			rows.Close()
			return nil, err
		}
		stuck = append(stuck, StuckCompleting{UploadID: id, StagedPath: staged.String})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(stuck) == 0 {
		return nil, nil
	}
	now := time.Now().UTC().Unix()
	if _, err := r.db.Exec(`UPDATE upload_sessions SET status = ?, staged_path = NULL, updated_at = ?
		WHERE status = ?`, UploadStatusActive, now, UploadStatusCompleting); err != nil {
		return nil, err
	}
	return stuck, nil
}

// ListUploadSessions returns all sessions (for startup reconciliation).
func (r *ClipRepository) ListUploadSessions() ([]*UploadSession, error) {
	rows, err := r.db.Query("SELECT " + uploadColumns + " FROM upload_sessions ORDER BY created_at ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*UploadSession
	for rows.Next() {
		var (
			id, filename, mime, status, env string
			fileSize                        int64
			chunkSize, totalChunks          int
			createdAt, updatedAt, expiresAt int64
			stagedPath, clipID, requestID   sql.NullString
			quotaReleased                   int
		)
		if err := rows.Scan(&id, &filename, &fileSize, &mime, &chunkSize, &totalChunks,
			&status, &env, &createdAt, &updatedAt, &expiresAt, &stagedPath, &clipID, &requestID,
			&quotaReleased); err != nil {
			return nil, err
		}
		out = append(out, &UploadSession{
			ID: id, Filename: filename, FileSize: fileSize, MimeType: mime,
			ChunkSize: chunkSize, TotalChunks: totalChunks, Status: status,
			EnvironmentID: env, RequestID: requestID.String,
			CreatedAt: createdAt, UpdatedAt: updatedAt,
			ExpiresAt: expiresAt, StagedPath: stagedPath.String, ClipID: clipID.String,
			QuotaReleased: quotaReleased != 0,
		})
	}
	return out, rows.Err()
}

// DeleteChunkRecords removes receipts for chunks whose files vanished (disk
// loss / partial cleanup) so GET accurately reports them as missing.
func (r *ClipRepository) DeleteChunkRecords(uploadID string, indices []int) error {
	if len(indices) == 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	for _, idx := range indices {
		if _, err := tx.Exec(
			"DELETE FROM upload_chunks WHERE upload_id = ? AND chunk_index = ?", uploadID, idx); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(
		"UPDATE upload_sessions SET updated_at = ? WHERE id = ?", nowUnix(), uploadID); err != nil {
		return err
	}
	return tx.Commit()
}

// RestoreChunkRecords re-adds receipts for chunk files found on disk but
// missing in DB (crash between file rename and DB commit).
func (r *ClipRepository) RestoreChunkRecords(uploadID string, indexToSize map[int]int64) error {
	if len(indexToSize) == 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	now := nowUnix()
	for idx, size := range indexToSize {
		if _, err := tx.Exec(`INSERT INTO upload_chunks (upload_id, chunk_index, size, received_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(upload_id, chunk_index) DO UPDATE SET size=excluded.size, received_at=excluded.received_at`,
			uploadID, idx, size, now); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(
		"UPDATE upload_sessions SET updated_at = ? WHERE id = ?", now, uploadID); err != nil {
		return err
	}
	return tx.Commit()
}

// UploadSessionExists is a cheap existence probe for orphan-dir sweeps.
func (r *ClipRepository) UploadSessionExists(uploadID string) (bool, error) {
	var one int
	err := r.db.QueryRow("SELECT 1 FROM upload_sessions WHERE id = ?", uploadID).Scan(&one)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// SetUploadExpiryForTest forces expires_at (timeout-path tests only).
func (r *ClipRepository) SetUploadExpiryForTest(uploadID string, expiresAtUnix int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.db.Exec("UPDATE upload_sessions SET expires_at = ? WHERE id = ?", expiresAtUnix, uploadID)
	return err
}
