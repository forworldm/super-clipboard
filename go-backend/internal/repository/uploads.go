// Package repository -- chunked upload sessions.
//
// Design notes (read before modifying):
//
//   - Sessions are persisted in SQLite (upload_sessions + upload_chunks) so a
//     power failure / process restart does not lose resume state. The byte
//     container is ONE file per session: it is pre-allocated by INIT at its
//     FINAL path (upload_sessions.staged_path, truncate(file_size)) and every
//     chunk is written in place at index*chunk_size. There are no per-chunk
//     files, no assembly step and no second copy of the bytes.
//   - upload_chunks is therefore the SINGLE source of truth for progress: a row
//     means "this byte range was written and synced". Nothing else is consulted
//     to answer "which chunks do I still need?".
//   - Concurrency: the global r.mu serialises short DB transactions only. File
//     IO (chunk writes, deletions) ALWAYS happens outside r.mu, and chunk writes
//     target disjoint byte ranges, so a slow disk never blocks unrelated
//     uploads/clips. Cross-operation races (PUT vs COMPLETE vs DELETE) are
//     resolved with atomic SQL predicates (e.g. UPDATE ... WHERE status='active')
//     plus the receipt table; see handlers for the protocol.
//   - Crash recovery: COMPLETE flips active->completing before inserting the
//     clip. The flip is a pure concurrency guard now (there is no long IO to
//     cover); a row left in `completing` by a power loss is rolled back to
//     `active` by startup reconciliation (ResetStuckCompleting), keeping its
//     pre-allocated file and its receipts.
//   - Quota: `completing` STILL HOLDS its reservation -- the flip is not a
//     release, and only the terminal transitions (complete success, abort,
//     expiry purge) call releaseQuotaTx. The ledger is therefore keyed on the
//     quota_released flag alone (reservationsHeldPredicate in quota.go), never on
//     the status column; see RecomputeUploadQuota for why a status filter there
//     would silently grant the bytes twice.
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
	StagedPath    string // pre-allocated FINAL file, created by init ("" only on legacy rows)
	ClipID        string // clip inserted by complete (empty only mid-upload)
	// QuotaReleased reports whether this session already returned its upload
	// quota reservation. The flag + a CAS UPDATE makes release idempotent:
	// abort/expire/complete can race, only one of them decrements the ledger.
	QuotaReleased bool
	// Clip params captured at init and turned into a row of `clips` by COMPLETE.
	// They are mandatory now (every upload is a clip), so a live session always
	// carries ClipExpiresAt; 0 only survives on rows written by older versions.
	ClipExpiresAt    int64
	ClipMaxDownloads *int
	ClipAccessCode   *string
	ClipAccessToken  *string
}

// IsExpired reports whether the session passed its resume TTL.
func (s *UploadSession) IsExpired(nowUnix int64) bool {
	if s == nil {
		return true
	}
	return nowUnix >= s.ExpiresAt
}

// AcceptsReplay reports whether a retried request may still be answered from
// this session instead of allocating a new one.
//
// A COMPLETED session is ALWAYS replayable, whatever its expires_at says: it is
// kept only as an idempotency record (its clip is the deliverable and it holds
// no reservation anymore), so expiry governs when the cleanup worker drops the
// row -- never whether a retry of init/complete/GET gets the same answer. Using
// expires_at here would turn the short replay window into a duplicate upload:
// the same (environment, requestId) would create a second session and a second
// clip. Live (active/completing) sessions still honour their resume TTL.
func (s *UploadSession) AcceptsReplay(nowUnix int64) bool {
	if s == nil {
		return false
	}
	if s.Status == UploadStatusCompleted {
		return true
	}
	return !s.IsExpired(nowUnix)
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

// ChunkOffset returns the byte offset a chunk index must be written at.
// It is derived from (index, chunkSize) only, so the offset is known as soon as
// init fixed the chunk size, i.e. before a single byte was transferred.
func (s *UploadSession) ChunkOffset(index int) (int64, bool) {
	if s == nil || index < 0 || index >= s.TotalChunks || s.ChunkSize <= 0 {
		return 0, false
	}
	return int64(index) * int64(s.ChunkSize), true
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
		clipExpiresAt                   sql.NullInt64
		clipMaxDownloads                sql.NullInt64
		clipAccessCode, clipAccessToken sql.NullString
	)
	if err := scan(&id, &filename, &fileSize, &mime, &chunkSize, &totalChunks,
		&status, &env, &createdAt, &updatedAt, &expiresAt, &stagedPath, &clipID, &requestID,
		&quotaReleased, &clipExpiresAt, &clipMaxDownloads, &clipAccessCode, &clipAccessToken); err != nil {
		return nil, err
	}
	session := &UploadSession{
		ID: id, Filename: filename, FileSize: fileSize, MimeType: mime,
		ChunkSize: chunkSize, TotalChunks: totalChunks, Status: status,
		EnvironmentID: env, RequestID: requestID.String,
		CreatedAt: createdAt, UpdatedAt: updatedAt,
		ExpiresAt: expiresAt, StagedPath: stagedPath.String, ClipID: clipID.String,
		QuotaReleased: quotaReleased != 0,
	}
	// Clip params are read unconditionally: they belong to every session now.
	// A row written by an older version simply reports ClipExpiresAt == 0 and is
	// refused by COMPLETE (it has no clip params to honour).
	if clipExpiresAt.Valid {
		session.ClipExpiresAt = clipExpiresAt.Int64
	}
	if clipMaxDownloads.Valid {
		v := int(clipMaxDownloads.Int64)
		session.ClipMaxDownloads = &v
	}
	if clipAccessCode.Valid && clipAccessCode.String != "" {
		code := clipAccessCode.String
		session.ClipAccessCode = &code
	}
	if clipAccessToken.Valid && clipAccessToken.String != "" {
		token := clipAccessToken.String
		session.ClipAccessToken = &token
	}
	return session, nil
}

const uploadColumns = `id, filename, file_size, mime_type, chunk_size, total_chunks,
	status, environment_id, created_at, updated_at, expires_at, staged_path, clip_id, request_id,
	quota_released, clip_expires_at, clip_max_downloads, clip_access_code, clip_access_token`

// CreateUploadSessionParams groups arguments for CreateUploadSession.
// Clip params come from the init request (already validated by the API layer)
// and are stored on the session; COMPLETE turns them into a clip row.
type CreateUploadSessionParams struct {
	Filename string
	FileSize int64
	MimeType string
	// EnvironmentID is required: every upload becomes a clip and a clip without
	// an owner would be unreachable (it is the only way to list a nameless clip).
	EnvironmentID    string
	RequestID        string // optional client idempotency key
	ChunkSize        int
	TTLSeconds       int
	ClipExpiresAt    int64
	ClipMaxDownloads *int
	ClipAccessCode   *string
	ClipAccessToken  *string
	// StagedPath is the FINAL storage path of the upload, chosen by the caller
	// (storage.FinalStoragePath) before the row exists. The file is created and
	// sized to FileSize right after the insert, so the session row and the byte
	// container are born together and COMPLETE never has to move the file.
	StagedPath string
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
	environmentID := strings.TrimSpace(params.EnvironmentID)
	if environmentID == "" {
		return nil, errors.New("environment id is required for every upload session")
	}
	// StagedPath stays optional at this layer: rows written by an older version
	// have none, and they are refused by COMPLETE (which needs the file). Every
	// session created by the API carries one from init.
	stagedPath := strings.TrimSpace(params.StagedPath)
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
	session := &UploadSession{
		ID: uuid.NewString(), Filename: filename, FileSize: params.FileSize,
		MimeType: mime, ChunkSize: chunkSize, TotalChunks: total,
		Status: UploadStatusActive, EnvironmentID: environmentID,
		RequestID: strings.TrimSpace(params.RequestID),
		// The byte container path is frozen at init: it is already the final clip
		// path, so COMPLETE inserts the clip against it without moving any byte.
		StagedPath: stagedPath,
		CreatedAt:  now, UpdatedAt: now, ExpiresAt: now + int64(ttl),
		// Clip params are stored for every session: COMPLETE always creates the
		// clip from them (there is no "regular file upload").
		ClipExpiresAt:    params.ClipExpiresAt,
		ClipMaxDownloads: params.ClipMaxDownloads,
		ClipAccessCode:   params.ClipAccessCode,
		ClipAccessToken:  params.ClipAccessToken,
	}
	return session, nil
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
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, 0, ?, ?, ?, ?)`,
		session.ID, session.Filename, session.FileSize, session.MimeType,
		session.ChunkSize, session.TotalChunks, session.Status,
		session.EnvironmentID, session.CreatedAt, session.UpdatedAt, session.ExpiresAt,
		nullIfEmpty(session.StagedPath), nullIfEmpty(session.RequestID),
		nullInt64Zero(session.ClipExpiresAt),
		nullIntPtr(session.ClipMaxDownloads),
		nullStringPtr(session.ClipAccessCode),
		nullStringPtr(session.ClipAccessToken),
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

// maxInitAttempts bounds the idempotency retries of CreateOrGetUploadSession.
// The first attempt is the normal path; the remaining ones only ever run after
// the insert lost a race on the (environment_id, request_id) UNIQUE index, so
// the INSERT executes at most maxInitAttempts times per call.
const maxInitAttempts = 3

// CreateOrGetUploadSession implements idempotent init on (env, requestID):
//   - a live session for the key is returned with created=false (replay);
//   - an expired session for the key is purged and replaced with a fresh one;
//   - without a requestID this always creates (no dedup possible).
//
// Two parallel inits race on the partial UNIQUE index: the loser re-reads the
// winner's row and returns it, so exactly one session consumes the key.
//
// Losing that race is RETRIED (bounded by maxInitAttempts) rather than failing
// the request: the loop head re-reads the key, so a retry either replays the
// winner or -- when the winner vanished between our failed insert and the retry
// (the cleanup worker purged an expired row) -- publishes this session and wins
// the key itself. Only a UNIQUE violation with a requestID set is retryable;
// every other outcome (validation error, DB/IO error, an expired key whose row
// cannot be purged yet because it is mid-merge) returns immediately, so the loop
// can never spin on a permanent condition.
func (r *ClipRepository) CreateOrGetUploadSession(params CreateUploadSessionParams) (session *UploadSession, created bool, err error) {
	requestID := strings.TrimSpace(params.RequestID)
	env := strings.TrimSpace(params.EnvironmentID)
	for attempt := 0; attempt < maxInitAttempts; attempt++ {
		if requestID != "" {
			existing, lookupErr := r.GetUploadSessionByRequestID(env, requestID)
			if lookupErr != nil {
				return nil, false, lookupErr
			}
			if existing != nil {
				if existing.AcceptsReplay(nowUnix()) {
					return existing, false, nil
				}
				// Expired key: purge the stale row so the fresh insert below can
				// reuse the index slot. File cleanup happens outside the repo.
				// An in-flight merge (`completing`) refuses to be aborted, and
				// then the key is still taken -- report it instead of replaying a
				// dead session (its /complete would only ever 409/410). This is a
				// permanent condition, not a race: do not retry it.
				if _, abortErr := r.AbortUploadSession(existing.ID); abortErr != nil &&
					!errors.Is(abortErr, sql.ErrNoRows) {
					return nil, false, &SessionUnavailableError{Session: existing}
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
		if requestID == "" || !isUniqueConstraintError(insertErr) {
			// No idempotency key to dedup on, or a genuine failure: surface it.
			return nil, false, insertErr
		}
		// Lost the insert race on (env, requestID): retry. Nothing was persisted
		// by the failed insert (no session row, no chunks), so the next
		// iteration re-reads the key and either replays the winner or creates the
		// session itself.
	}
	// The retries were exhausted, which means another init (or a purge) keeps
	// taking or dropping the key in between: report the state of the key instead
	// of guessing -- replay of the winner, SessionUnavailableError for an
	// expired-but-unpurgeable winner, or a clear idempotency conflict when no
	// row carries the key anymore.
	return r.replayExistingSession(env, requestID)
}

// replayExistingSession loads the session that owns an idempotency key and
// decides whether it may be replayed: a live one is returned as-is (idempotent
// init), while one that already expired is only replayable if its row vanished
// in the meantime (cleanup raced us) -- otherwise the client must retry later.
func (r *ClipRepository) replayExistingSession(env string, requestID string) (*UploadSession, bool, error) {
	existing, lookupErr := r.GetUploadSessionByRequestID(env, requestID)
	if lookupErr != nil {
		return nil, false, lookupErr
	}
	if existing == nil {
		return nil, false, errors.New("unable to create upload session (idempotency conflict)")
	}
	if !existing.AcceptsReplay(nowUnix()) {
		return nil, false, &SessionUnavailableError{Session: existing}
	}
	return existing, false, nil
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

// MarkChunkReceived upserts a chunk receipt and bumps updated_at. This is the
// commit point of a chunk: the bytes were already written (and synced) into the
// session's pre-allocated file at index*chunk_size, so the row is what turns a
// byte range into "received".
//
// The recorded size MUST equal the chunk's byte range: every chunk but the last
// one carries exactly chunk_size bytes, the last one carries the remainder. A
// mismatching receipt is refused (ErrChunkSizeMismatch) instead of being stored,
// so the table can never claim a range that was not fully written.
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
	var total, chunkSize int
	var fileSize int64
	if err := tx.QueryRow(
		"SELECT status, total_chunks, chunk_size, file_size FROM upload_sessions WHERE id = ?", uploadID).
		Scan(&status, &total, &chunkSize, &fileSize); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sql.ErrNoRows
		}
		return err
	}
	if status == UploadStatusCompleted {
		return ErrUploadAlreadyCompleted
	}
	if index >= total || chunkSize <= 0 {
		return ErrChunkIndexOutOfRange
	}
	probe := &UploadSession{ChunkSize: chunkSize, TotalChunks: total, FileSize: fileSize}
	expected, ok := probe.ExpectedChunkSize(index)
	if !ok {
		return ErrChunkIndexOutOfRange
	}
	if size != expected {
		return &ChunkSizeMismatchError{Index: index, Got: size, Want: expected}
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

// ChunkSizeMismatchError reports a chunk whose payload did not match its byte
// range (non-last chunks: exactly chunk_size; last chunk: the remainder).
type ChunkSizeMismatchError struct {
	Index int
	Got   int64
	Want  int64
}

func (e *ChunkSizeMismatchError) Error() string {
	return "chunk size does not match its byte range"
}

// TryBeginComplete atomically transitions active->completing after verifying
// every chunk of the session has a receipt in the database.
//
// The transition only guards against concurrent completers (it no longer covers
// any assembly IO: the bytes already sit in their final place). On success the
// caller inserts the clip and flips to completed; on failure the session stays
// active so the client can resume.
func (r *ClipRepository) TryBeginComplete(uploadID string) (*UploadSession, []int, error) {
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
	res, err := tx.Exec(`UPDATE upload_sessions SET status = ?, updated_at = ?
		WHERE id = ? AND status = ?`,
		UploadStatusCompleting, now, uploadID, UploadStatusActive)
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
	session.UpdatedAt = now
	sort.Ints(received)
	return session, received, nil
}

// SessionUnavailableError means an idempotency key is still held by a session
// that is already expired but cannot be purged yet (it is `completing`, i.e. a
// COMPLETE may be inserting its clip, or the row is inside the purge grace
// window). Returning that stale row would hand the client a 200 with an unusable
// uploadId, so the caller refuses instead and lets the client retry once the
// transition settled.
type SessionUnavailableError struct{ Session *UploadSession }

func (e *SessionUnavailableError) Error() string { return "upload session is not usable yet" }

// SessionExpiredError / SessionCompletedError / SessionCompletingError carry
// the loaded session so handlers can render precise statuses.
type SessionExpiredError struct{ Session *UploadSession }

func (e *SessionExpiredError) Error() string { return "upload session expired" }

type SessionCompletedError struct{ Session *UploadSession }

func (e *SessionCompletedError) Error() string { return "upload already completed" }

type SessionCompletingError struct{ Session *UploadSession }

func (e *SessionCompletingError) Error() string { return "upload is completing" }

// completedExpiry returns the expiry a successful session keeps for idempotent
// replay: now + ttl, never later than the expiry it already had (a client may
// have asked for a shorter resume window than the configured replay window).
func completedExpiry(nowUnix int64, currentExpiry int64, ttlSeconds int) int64 {
	if ttlSeconds <= 0 {
		return currentExpiry
	}
	candidate := nowUnix + int64(ttlSeconds)
	if currentExpiry > 0 && currentExpiry < candidate {
		return currentExpiry
	}
	return candidate
}

func nullIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func nullInt64Zero(v int64) interface{} {
	if v == 0 {
		return nil
	}
	return v
}

func nullIntPtr(p *int) interface{} {
	if p == nil {
		return nil
	}
	return *p
}

func nullStringPtr(p *string) interface{} {
	if p == nil || *p == "" {
		return nil
	}
	return *p
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
	var (
		fileSize  int64
		expiresAt int64
	)
	if err := tx.QueryRow(
		"SELECT file_size, expires_at FROM upload_sessions WHERE id = ?", uploadID).Scan(&fileSize, &expiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("upload session not in completing state (aborted or expired?)")
		}
		return nil, err
	}
	// A finished session stops being a resume candidate, so it drops the long
	// resume TTL: expires_at shrinks to the (much shorter) idempotency window.
	// Only the row is affected -- the clip and its file are independent of it --
	// which keeps finished rows, and their (environment_id, request_id) slot,
	// from lingering for a whole day.
	res, err := tx.Exec(`UPDATE upload_sessions SET status = ?, clip_id = ?, updated_at = ?, expires_at = ?
		WHERE id = ? AND status = ?`,
		UploadStatusCompleted, nullIfEmpty(clipID), now, completedExpiry(now, expiresAt, r.settings.EffectiveCompletedUploadTTL()),
		uploadID, UploadStatusCompleting)
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

// FailComplete rolls completing->active so a failed clip insert can be retried.
// The pre-allocated file and the chunk receipts are KEPT: the bytes are already
// in their final place, so nothing has to be undone -- the client can simply
// call COMPLETE again (or abort). Returns the storage path (which stays valid).
func (r *ClipRepository) FailComplete(uploadID string) (string, error) {
	now := nowUnix()
	r.mu.Lock()
	defer r.mu.Unlock()
	tx, err := r.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback() //nolint:errcheck
	var staged sql.NullString
	if err := tx.QueryRow(
		"SELECT staged_path FROM upload_sessions WHERE id = ?", uploadID).Scan(&staged); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil // already aborted: nothing to roll back
		}
		return "", err
	}
	if _, err := tx.Exec(`UPDATE upload_sessions SET status = ?, updated_at = ?
		WHERE id = ? AND status = ?`,
		UploadStatusActive, now, uploadID, UploadStatusCompleting); err != nil {
		return "", err
	}
	// The session goes back to `active` and KEEPS its bytes on disk, so it must
	// keep its quota reservation as well: releasing here would leave live bytes
	// unaccounted for (a quota bypass for up to the session TTL) and contradict
	// RecomputeUploadQuota, which sums file_size over every session that still
	// holds a reservation (quota_released = 0, i.e. `active` AND `completing`).
	// The reservation is returned by the terminal transitions only: complete
	// success, DELETE (abort) or expiry purge.
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return staged.String, nil
}

// AbortUploadSession deletes the session + chunk rows atomically and returns
// the session for file cleanup outside the lock. Completed sessions linked to
// a clip are NOT abortable (delete the clip instead); a `completing` session is
// also refused so a cancel can never tear down a COMPLETE whose clip INSERT (and
// its pending completing->completed flip) is in flight — such a session rolls
// back to `active` on failure (FailComplete) or at startup
// (ResetStuckCompleting), and the caller can retry DELETE after that. In both
// cases the reservation stays where it belongs: with the session row.
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

// CompletingPurgeGraceSeconds is how long an EXPIRED `completing` row is
// protected from the destructive expiry purge.
//
// `completing` is a reservation holder (see reservationsHeldPredicate), so a row
// left there by a crash keeps its bytes reserved. Skipping such rows FOREVER --
// as this function used to -- meant the reservation could only come back at the
// next process start (ResetStuckCompleting); on a long-running server those
// bytes stayed reserved for good. The grace window is what makes the reclaim
// safe: the flip is taken right before the clip INSERT and every chunk was
// already written (and fsynced) during PUT, so COMPLETE performs no file IO
// after it and cannot plausibly still be running minutes later.
const CompletingPurgeGraceSeconds int64 = 900 // 15 min

// PurgeExpiredUploads deletes expired sessions and returns victims for file
// cleanup outside the lock. Completed sessions with a clip keep their clip file;
// only the session row + chunk receipts are dropped.
//
// Expired `completing` rows are purged only once they are older than
// CompletingPurgeGraceSeconds (updated_at is refreshed by the active->completing
// flip), because a COMPLETE that is still in flight is about to insert its clip
// and unlinking its file would leave that clip pointing at nothing. The grace
// window is the guard; as a second guard the victim reports HasClip when a clip
// row already references the staged path, so the file survives even if the purge
// interleaves between the clip INSERT and the completing->completed flip.
func (r *ClipRepository) PurgeExpiredUploads(nowUnixSec int64) ([]UploadPurgeVictim, error) {
	return r.PurgeExpiredUploadsWithGrace(nowUnixSec, CompletingPurgeGraceSeconds)
}

// PurgeExpiredUploadsWithGrace is PurgeExpiredUploads with an explicit grace
// window (<= 0 purges a stale `completing` row immediately). Tests use it to
// drive the window deterministically.
func (r *ClipRepository) PurgeExpiredUploadsWithGrace(nowUnixSec int64, completingGraceSeconds int64) ([]UploadPurgeVictim, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := nowUnixSec - completingGraceSeconds
	rows, err := r.db.Query(`
		SELECT s.id, s.staged_path, s.file_size,
			CASE WHEN s.clip_id IS NOT NULL AND s.clip_id <> '' THEN 1
				WHEN s.staged_path IS NOT NULL AND EXISTS (
					SELECT 1 FROM clips c WHERE c.file_path = s.staged_path) THEN 1
				ELSE 0 END AS has_clip
		FROM upload_sessions s
		WHERE s.expires_at <= ? AND (s.status <> ? OR s.updated_at <= ?)`,
		nowUnixSec, UploadStatusCompleting, cutoff)
	if err != nil {
		return nil, err
	}
	var victims []UploadPurgeVictim
	for rows.Next() {
		var id string
		var fileSize int64
		var hasClip int64
		var staged sql.NullString
		if err := rows.Scan(&id, &staged, &fileSize, &hasClip); err != nil {
			rows.Close()
			return nil, err
		}
		victims = append(victims, UploadPurgeVictim{
			UploadID: id, StagedPath: staged.String, HasClip: hasClip != 0,
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

// ResetStuckCompleting rolls every `completing` row back to `active` (a power
// loss between the CAS and the clip insert). The pre-allocated file and the
// chunk receipts stay untouched: they are still exactly what the client needs to
// retry COMPLETE, so recovery is a single status update with no file IO.
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
	if _, err := r.db.Exec(`UPDATE upload_sessions SET status = ?, updated_at = ?
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
		session, err := scanUploadSession(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, session)
	}
	return out, rows.Err()
}

// ClearChunkReceipts drops every receipt of a session. It is the recovery path
// for a lost/re-created byte container: the pre-allocated file is truncated back
// to zero content, so no range can be trusted anymore and the whole upload has
// to be sent again. (The old disk-scanning reconcile -- "DB says yes, disk says
// no" -- disappears with the per-chunk files.)
func (r *ClipRepository) ClearChunkReceipts(uploadID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.Exec("DELETE FROM upload_chunks WHERE upload_id = ?", uploadID); err != nil {
		return err
	}
	if _, err := tx.Exec(
		"UPDATE upload_sessions SET updated_at = ? WHERE id = ?", nowUnix(), uploadID); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteChunkReceipt drops the receipt of a single chunk index. It is used when
// a retried PUT left a partially overwritten (or oversized) range: the range must
// not be advertised as complete, so the client re-sends that chunk.
func (r *ClipRepository) DeleteChunkReceipt(uploadID string, index int) error {
	if strings.TrimSpace(uploadID) == "" || index < 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.db.Exec(
		"DELETE FROM upload_chunks WHERE upload_id = ? AND chunk_index = ?", uploadID, index)
	return err
}

// ReferencedFilePaths returns every storage path that is still owned by a live
// upload session or by an existing clip. Startup reconciliation uses it to sweep
// files that no row points at (a crash between INSERT and file creation, or a
// hand-copied leftovers) without ever touching clip data.
func (r *ClipRepository) ReferencedFilePaths() (map[string]bool, error) {
	out := map[string]bool{}
	rows, err := r.db.Query("SELECT staged_path FROM upload_sessions WHERE staged_path IS NOT NULL")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var path sql.NullString
		if err := rows.Scan(&path); err != nil {
			rows.Close()
			return nil, err
		}
		if path.Valid && path.String != "" {
			out[path.String] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	clipRows, err := r.db.Query("SELECT file_path FROM clips WHERE file_path IS NOT NULL")
	if err != nil {
		return nil, err
	}
	defer clipRows.Close()
	for clipRows.Next() {
		var path sql.NullString
		if err := clipRows.Scan(&path); err != nil {
			return nil, err
		}
		if path.Valid && path.String != "" {
			out[path.String] = true
		}
	}
	if err := clipRows.Err(); err != nil {
		return nil, err
	}
	return out, nil
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

// AgeCompletingForTest pushes updated_at of a `completing` session into the
// past. updated_at is the clock the `completing` purge grace is measured
// against (see CompletingPurgeGraceSeconds), so this is how tests simulate a
// COMPLETE that died mid-flight. Returns false when the row is not completing.
func (r *ClipRepository) AgeCompletingForTest(uploadID string, ageSeconds int64) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	res, err := r.db.Exec("UPDATE upload_sessions SET updated_at = ? WHERE id = ? AND status = ?",
		nowUnix()-ageSeconds, uploadID, UploadStatusCompleting)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

// SetUploadExpiryForTest forces expires_at (timeout-path tests only).
func (r *ClipRepository) SetUploadExpiryForTest(uploadID string, expiresAtUnix int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.db.Exec("UPDATE upload_sessions SET expires_at = ? WHERE id = ?", expiresAtUnix, uploadID)
	return err
}
