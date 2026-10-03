package api

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pixia1234/super-clipboard/backend/internal/apperr"
	"github.com/pixia1234/super-clipboard/backend/internal/models"
	"github.com/pixia1234/super-clipboard/backend/internal/repository"
	"github.com/pixia1234/super-clipboard/backend/internal/schemas"
	"github.com/pixia1234/super-clipboard/backend/internal/storage"
	"github.com/pixia1234/super-clipboard/backend/internal/utils"
)

// ---------------------------------------------------------------------------
// Chunked uploads: POST /api/uploads/init, PUT .../chunks/{index},
// GET /api/uploads/{id}, POST .../complete, DELETE /api/uploads/{id}.
//
// Protocol guarantees:
//   - uploadId / chunkSize / totalChunks are server-generated (init).
//   - chunks are raw bytes (no base64, no giant JSON), idempotent rewrites.
//   - sessions persist in SQLite; chunk bytes persist on disk for resume
//     across network drops and server restarts.
//   - no global lock is held during file IO (see repository docs).
// ---------------------------------------------------------------------------

func uploadIDFromRequest(r *http.Request) string {
	// Route registers {id}; be tolerant to {upload_id} naming too.
	if v := routeParam(r, "id"); v != "" {
		return v
	}
	return routeParam(r, "upload_id")
}

// expireUploadNow deletes an expired session (DB under short lock, files outside)
// and reports whether a row was actually removed.
func (a *App) expireUploadNow(session *repository.UploadSession) {
	if session == nil {
		return
	}
	aborted, err := a.Repo.AbortUploadSession(session.ID)
	if err != nil {
		// Already gone or completed-with-clip: nothing to do.
		return
	}
	_ = storage.RemoveUploadDir(a.Settings.FileStorageDir, session.ID)
	if aborted.StagedPath != "" && aborted.ClipID == "" {
		_ = os.Remove(aborted.StagedPath)
		_ = os.Remove(aborted.StagedPath + ".part")
	}
	// Best-effort: remove any leftover .part.* temps for this staged file.
	if aborted.StagedPath != "" {
		matches, _ := filepath.Glob(aborted.StagedPath + ".part.*")
		for _, m := range matches {
			_ = os.Remove(m)
		}
	}
}

// loadActiveUpload loads session+chunks, handling 404/410 mapping.
// When expired it synchronously purges (rollback) and returns ok=false with
// expired=true so handlers can emit 410.
func (a *App) loadActiveUpload(w http.ResponseWriter, uploadID string) (session *repository.UploadSession, received []int, ok bool, expired bool) {
	trimmed := strings.TrimSpace(uploadID)
	if trimmed == "" {
		writeError(w, newHTTPError(http.StatusNotFound, "上传会话不存在"))
		return nil, nil, false, false
	}
	s, r, err := a.Repo.GetUploadSessionWithChunks(trimmed)
	if err != nil {
		writeError(w, err)
		return nil, nil, false, false
	}
	if s == nil {
		writeError(w, newHTTPError(http.StatusNotFound, "上传会话不存在"))
		return nil, nil, false, false
	}
	if s.IsExpired(time.Now().UTC().Unix()) {
		a.expireUploadNow(s)
		writeError(w, newHTTPError(http.StatusGone, "上传会话已过期，请重新上传"))
		return nil, nil, false, true
	}
	return s, r, true, false
}

// reconcileDiskState makes DB receipts match files actually on disk (outside the
// global lock for IO, brief locks for DB fixes). It returns the corrected
// received list. Used by GET (resume accuracy) and startup recovery.
func (a *App) reconcileDiskState(session *repository.UploadSession, received []int) []int {
	if session == nil || session.Status == repository.UploadStatusCompleted {
		return received
	}
	onDisk, err := storage.ListChunkFilesOnDisk(a.Settings.FileStorageDir, session.ID)
	if err != nil {
		a.logger.Printf("ERROR:    unable to scan upload dir %s: %v", session.ID, err)
		return received
	}
	want := make(map[int]bool, session.TotalChunks)
	for i := 0; i < session.TotalChunks; i++ {
		want[i] = true
	}
	haveDB := make(map[int]bool, len(received))
	for _, idx := range received {
		haveDB[idx] = true
	}
	var toDelete []int
	for idx := range haveDB {
		if !want[idx] {
			toDelete = append(toDelete, idx) // stale index (should not happen)
			continue
		}
		if _, ok := onDisk[idx]; !ok {
			toDelete = append(toDelete, idx) // DB says yes, disk says no
		}
	}
	restore := make(map[int]int64)
	for idx, size := range onDisk {
		if !want[idx] {
			continue
		}
		if !haveDB[idx] {
			// Crash between file rename and DB commit: file is durable, restore receipt.
			restore[idx] = size
		}
	}
	if len(toDelete) > 0 {
		if err := a.Repo.DeleteChunkRecords(session.ID, toDelete); err != nil {
			a.logger.Printf("ERROR:    unable to prune missing chunks for %s: %v", session.ID, err)
		} else {
			pruned := make(map[int]bool, len(toDelete))
			for _, idx := range toDelete {
				pruned[idx] = true
			}
			kept := received[:0]
			for _, idx := range received {
				if !pruned[idx] {
					kept = append(kept, idx)
				}
			}
			received = kept
		}
	}
	if len(restore) > 0 {
		if err := a.Repo.RestoreChunkRecords(session.ID, restore); err != nil {
			a.logger.Printf("ERROR:    unable to restore chunk receipts for %s: %v", session.ID, err)
		} else {
			for idx := range restore {
				received = append(received, idx)
			}
			sort.Ints(received)
		}
	}
	return received
}

func toMillis(unixSec int64) int64 { return unixSec * 1000 }

// verifyCaptcha mirrors the captcha gate in handleCreateClip. It performs the
// (network) verification call; callers invoke it BEFORE any storage is
// consumed by the request.
func (a *App) verifyCaptcha(w http.ResponseWriter, r *http.Request, token *string, provider *string) bool {
	if !a.Settings.CaptchaEnabled() {
		return true
	}
	if !a.Settings.HasCaptchaSecret() {
		writeError(w, newHTTPError(http.StatusInternalServerError, "验证码服务未正确配置"))
		return false
	}
	clientIP := utils.ExtractClientIP(r)
	// 避免代理导致的内网地址与浏览器求解 IP 不一致
	if utils.IsPrivateAddress(clientIP) {
		clientIP = ""
	}
	if err := utils.VerifyCaptchaToken(r.Context(), token, utils.CaptchaOptions{
		Provider:    a.Settings.CaptchaProvider,
		Secret:      a.Settings.CaptchaSecret,
		RemoteIP:    clientIP,
		Timeout:     time.Duration(a.Settings.CaptchaTimeoutSeconds * float64(time.Second)),
		BypassToken: a.Settings.CaptchaBypassToken,
		Client:      a.client,
	}); err != nil {
		writeError(w, err)
		return false
	}
	return true
}

// freeDiskSpace probes the free bytes of the volume holding path. Tests inject
// a deterministic probe through App.freeBytesFn; production uses statfs.
func (a *App) freeDiskSpace(path string) (int64, error) {
	if a.freeBytesFn != nil {
		return a.freeBytesFn(path)
	}
	return defaultFreeDiskBytes(path)
}

// checkDiskWatermark renders a typed 507 when the storage volume dropped below
// MinFreeDiskBytes. It reports false once the response has been written.
func (a *App) checkDiskWatermark(w http.ResponseWriter) bool {
	minFree := a.Settings.EffectiveMinFreeDiskBytes()
	if minFree <= 0 {
		return true // watermark disabled
	}
	free, err := a.freeDiskSpace(a.Settings.FileStorageDir)
	if err != nil {
		// Unknown free space must not stop the service: the upload quota ledger
		// still bounds total consumption.
		a.logger.Printf("WARN:    unable to read free disk space: %v", err)
		return true
	}
	if err := repository.CheckDiskWatermark(free, minFree); err != nil {
		writeError(w, err)
		return false
	}
	return true
}

// POST /api/uploads/init
//
// Ordering matters: fast idempotent replay first (a retried init must not be
// blocked by the already-consumed captcha token), then captcha BEFORE any
// session/disk is created (unverified clients can never consume storage),
// then the global storage gates (session cap, quota reservation, disk
// watermark), then a race-safe insert keyed on (environment, requestId).
func (a *App) handleInitUpload(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	req, validationError := schemas.ParseUploadInitRequest(body)
	if validationError != nil {
		writeError(w, validationError)
		return
	}
	if req.FileSize > a.Settings.MaxFileSizeBytes {
		writeError(w, newHTTPError(http.StatusBadRequest, "文件体积超过限制"))
		return
	}
	if req.FileSize < 0 {
		writeError(w, newHTTPError(http.StatusBadRequest, "文件大小无效"))
		return
	}

	// Idempotent replay: an existing live session for (env, requestId) is
	// returned as-is (200 + its receivedChunks). Highly desired: network retry
	// of init never leaks a second session dir onto disk.
	if req.RequestID != "" {
		existing, err := a.Repo.GetUploadSessionByRequestID(req.EnvironmentID, req.RequestID)
		if err != nil {
			writeError(w, err)
			return
		}
		if existing != nil {
			if !existing.IsExpired(time.Now().UTC().Unix()) {
				received, listErr := a.Repo.ListReceivedChunks(existing.ID)
				if listErr != nil || received == nil {
					received = []int{}
				}
				writeJSON(w, http.StatusOK, schemas.UploadInitResponse{
					UploadID: existing.ID, ChunkSize: existing.ChunkSize, TotalChunks: existing.TotalChunks,
					FileSize: existing.FileSize, Filename: existing.Filename, MimeType: existing.MimeType,
					ExpiresAt: toMillis(existing.ExpiresAt), Status: existing.Status,
					ReceivedChunks: received,
				})
				return
			}
			// Expired key: purge rows AND chunk files so a fresh session can
			// reuse the idempotency slot without orphaning old bytes.
			a.expireUploadNow(existing)
		}
	}

	// Captcha gate: verified once, up-front, before any chunk can be stored.
	// Turnstile tokens are single-use & short-lived, so they belong at init.
	if !a.verifyCaptcha(w, r, req.CaptchaToken, req.CaptchaProvider) {
		return
	}

	// ---- Global storage gates. Fixed order: live-session cap, then quota
	// reservation (atomic CAS in the DB), then the free-disk watermark. All of
	// them run before the session row exists, so a refusal leaves nothing
	// behind: no session, no bytes on disk, no dangling reservation.
	quotaLimit := a.Settings.EffectiveUploadQuota()
	sessionLimit := a.Settings.EffectiveMaxActiveUploadSessions()
	if sessionLimit > 0 {
		active, err := a.Repo.CountActiveUploadSessions()
		if err != nil {
			writeError(w, err)
			return
		}
		if active >= int64(sessionLimit) {
			writeError(w, apperr.NewStorageError(apperr.StorageCodeSessionLimit,
				"活动上传会话数已达上限：%d / %d，请完成或取消进行中的上传后重试", active, sessionLimit))
			return
		}
	}

	if err := a.Repo.ReserveUploadQuota(req.FileSize, quotaLimit); err != nil {
		writeError(w, err)
		return
	}
	// Until a session row owns the reservation it is ours to give back.
	reservationHeld := true
	releaseReservation := func() {
		if !reservationHeld {
			return
		}
		reservationHeld = false
		if _, err := a.Repo.CompensateUploadQuota(req.FileSize); err != nil {
			a.logger.Printf("ERROR:    unable to roll back %d byte upload quota reservation: %v", req.FileSize, err)
		}
	}
	defer releaseReservation() // safety net for every early return below

	if !a.checkDiskWatermark(w) {
		releaseReservation()
		return
	}

	chunkSize := a.Settings.EffectiveChunkSize()
	ttl := a.Settings.EffectiveUploadTTL()
	session, created, err := a.Repo.CreateOrGetUploadSession(repository.CreateUploadSessionParams{
		Filename: req.Filename, FileSize: req.FileSize, MimeType: req.MimeType,
		EnvironmentID: req.EnvironmentID, RequestID: req.RequestID,
		ChunkSize: chunkSize, TTLSeconds: ttl,
	})
	if err != nil {
		releaseReservation()
		writeError(w, err)
		return
	}
	if created {
		// The session row owns the reservation from here on; abort, expiry
		// cleanup, complete rollback and complete success all return it through
		// the idempotent quota_released CAS.
		reservationHeld = false
	}
	if !created {
		// Lost the insert race or an exact-replica row appeared in between: the
		// winning session already holds a reservation, so give ours back (a
		// replayed init must never reserve twice).
		releaseReservation()
		received, listErr := a.Repo.ListReceivedChunks(session.ID)
		if listErr != nil || received == nil {
			received = []int{}
		}
		writeJSON(w, http.StatusOK, schemas.UploadInitResponse{
			UploadID: session.ID, ChunkSize: session.ChunkSize, TotalChunks: session.TotalChunks,
			FileSize: session.FileSize, Filename: session.Filename, MimeType: session.MimeType,
			ExpiresAt: toMillis(session.ExpiresAt), Status: session.Status,
			ReceivedChunks: received,
		})
		return
	}
	// Directory creation is IO outside the repository lock. On failure roll
	// back the DB row so no orphan session remains.
	if err := storage.EnsureUploadDir(a.Settings.FileStorageDir, session.ID); err != nil {
		_, _ = a.Repo.AbortUploadSession(session.ID)
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, schemas.UploadInitResponse{
		UploadID: session.ID, ChunkSize: session.ChunkSize, TotalChunks: session.TotalChunks,
		FileSize: session.FileSize, Filename: session.Filename, MimeType: session.MimeType,
		ExpiresAt: toMillis(session.ExpiresAt), Status: session.Status,
		ReceivedChunks: []int{},
	})
}

// PUT /api/uploads/{id}/chunks/{index} -- raw chunk bytes.
func (a *App) handlePutChunk(w http.ResponseWriter, r *http.Request) {
	uploadID := uploadIDFromRequest(r)
	indexRaw := routeParam(r, "index")
	index, err := strconv.Atoi(strings.TrimSpace(indexRaw))
	if err != nil || index < 0 {
		writeError(w, newHTTPError(http.StatusBadRequest, "分片序号无效"))
		return
	}
	session, received, ok, _ := a.loadActiveUpload(w, uploadID)
	if !ok {
		return
	}
	if session.Status == repository.UploadStatusCompleted {
		writeError(w, newHTTPError(http.StatusConflict, "上传已完成"))
		return
	}
	if session.Status == repository.UploadStatusCompleting {
		writeError(w, newHTTPError(http.StatusConflict, "上传正在合并，请稍后重试"))
		return
	}
	if session.TotalChunks == 0 {
		writeError(w, newHTTPError(http.StatusBadRequest, "空文件无需上传分片，请直接完成"))
		return
	}
	if index >= session.TotalChunks {
		writeError(w, newHTTPError(http.StatusBadRequest, "分片序号超出范围"))
		return
	}
	expected, _ := session.ExpectedChunkSize(index)
	// Fast-path 413 when Content-Length already exceeds expectation.
	if r.ContentLength > expected {
		writeError(w, newHTTPError(http.StatusRequestEntityTooLarge, "分片体积超过限制"))
		return
	}
	if r.Body == nil {
		writeError(w, newHTTPError(http.StatusBadRequest, "分片内容缺失"))
		return
	}
	// Disk watermark: never stream bytes onto a volume below the free-space
	// floor. Checked before the first byte is written so nothing lands on disk.
	if !a.checkDiskWatermark(w) {
		return
	}
	// Stream to disk (no global lock). Limit to expected size; overflow is 413.
	// Client cancellation surfaces as a read error -> 499-style abort without
	// touching DB, leaving the session resumable.
	defer r.Body.Close()
	stored, err := storage.WriteChunkStream(a.Settings.FileStorageDir, session.ID, index, r.Body, expected)
	if err != nil {
		if errors.Is(err, storage.ErrChunkTooLarge) {
			writeError(w, newHTTPError(http.StatusRequestEntityTooLarge, "分片体积超过限制"))
			return
		}
		// Distinguish client disconnect (context cancelled) from server IO errors.
		if r.Context().Err() != nil {
			// Client went away; nothing to render (connection dead). Just ensure
			// no partial chunk lingers (WriteChunkStream already cleaned temp).
			return
		}
		a.logger.Printf("ERROR:    unable to store chunk %s/%d: %v", session.ID, index, err)
		writeError(w, newHTTPError(http.StatusInternalServerError, "分片存储失败，请重试"))
		return
	}
	if stored != expected {
		// Undersize (truncated body / wrong slice): drop the bad file, keep resumable.
		_ = os.Remove(storage.ChunkFilePath(a.Settings.FileStorageDir, session.ID, index))
		writeError(w, newHTTPError(http.StatusBadRequest, "分片大小不匹配，请重试"))
		return
	}
	// Durable file -> brief DB upsert (idempotent rewrite for retries).
	if err := a.Repo.MarkChunkReceived(session.ID, index, stored); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			_ = os.Remove(storage.ChunkFilePath(a.Settings.FileStorageDir, session.ID, index))
			writeError(w, newHTTPError(http.StatusNotFound, "上传会话不存在"))
			return
		}
		// Session completed concurrently: chunk file is now orphan; best-effort remove.
		if strings.Contains(err.Error(), "already completed") {
			_ = os.Remove(storage.ChunkFilePath(a.Settings.FileStorageDir, session.ID, index))
			writeError(w, newHTTPError(http.StatusConflict, "上传已完成"))
			return
		}
		a.logger.Printf("ERROR:    unable to record chunk %s/%d: %v", session.ID, index, err)
		_ = os.Remove(storage.ChunkFilePath(a.Settings.FileStorageDir, session.ID, index))
		writeError(w, err)
		return
	}
	// Re-list for an accurate progress response (cheap indexed read).
	updated, err := a.Repo.ListReceivedChunks(session.ID)
	if err != nil {
		updated = append(append([]int{}, received...), index)
		sort.Ints(updated)
		// Deduplicate in case of rewrite.
		dedup := updated[:0]
		var prev = -1
		for _, v := range updated {
			if v != prev {
				dedup = append(dedup, v)
				prev = v
			}
		}
		updated = dedup
	}
	writeJSON(w, http.StatusOK, schemas.UploadChunkResponse{
		UploadID: session.ID, Index: index, ReceivedCount: len(updated),
		TotalChunks: session.TotalChunks, ReceivedChunks: updated,
		Complete: len(updated) >= session.TotalChunks,
	})
}

// GET /api/uploads/{id} -- resume info.
func (a *App) handleGetUpload(w http.ResponseWriter, r *http.Request) {
	uploadID := uploadIDFromRequest(r)
	session, received, ok, _ := a.loadActiveUpload(w, uploadID)
	if !ok {
		return
	}
	// Completed sessions: report stored state (no disk scan needed).
	if session.Status == repository.UploadStatusCompleted {
		writeJSON(w, http.StatusOK, schemas.UploadInfoResponse{
			UploadID: session.ID, Filename: session.Filename, FileSize: session.FileSize,
			MimeType: session.MimeType, ChunkSize: session.ChunkSize, TotalChunks: session.TotalChunks,
			ReceivedChunks: received, ReceivedCount: len(received), MissingChunks: []int{},
			Status: session.Status, CreatedAt: toMillis(session.CreatedAt),
			UpdatedAt: toMillis(session.UpdatedAt), ExpiresAt: toMillis(session.ExpiresAt),
		})
		return
	}
	received = a.reconcileDiskState(session, received)
	missing := repository.MissingChunks(session.TotalChunks, received)
	if missing == nil {
		missing = []int{}
	}
	if received == nil {
		received = []int{}
	}
	writeJSON(w, http.StatusOK, schemas.UploadInfoResponse{
		UploadID: session.ID, Filename: session.Filename, FileSize: session.FileSize,
		MimeType: session.MimeType, ChunkSize: session.ChunkSize, TotalChunks: session.TotalChunks,
		ReceivedChunks: received, ReceivedCount: len(received), MissingChunks: missing,
		Status: session.Status, CreatedAt: toMillis(session.CreatedAt),
		UpdatedAt: toMillis(session.UpdatedAt), ExpiresAt: toMillis(session.ExpiresAt),
	})
}

// DELETE /api/uploads/{id} -- abort + cleanup (cancel path).
func (a *App) handleAbortUpload(w http.ResponseWriter, r *http.Request) {
	uploadID := strings.TrimSpace(uploadIDFromRequest(r))
	if uploadID == "" {
		writeError(w, newHTTPError(http.StatusNotFound, "上传会话不存在"))
		return
	}
	session, err := a.Repo.AbortUploadSession(uploadID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, newHTTPError(http.StatusNotFound, "上传会话不存在"))
			return
		}
		var completing *repository.SessionCompletingError
		if errors.As(err, &completing) {
			writeError(w, newHTTPError(http.StatusConflict, "上传正在合并，无法取消"))
			return
		}
		var completed *repository.SessionCompletedError
		if errors.As(err, &completed) {
			writeError(w, newHTTPError(http.StatusConflict, "上传已完成，无法取消"))
			return
		}
		writeError(w, err)
		return
	}
	// Files outside the lock. Completed-with-clip is rejected above, so a
	// staged path here is always safe to delete (file-only session).
	_ = storage.RemoveUploadDir(a.Settings.FileStorageDir, uploadID)
	if session.StagedPath != "" && session.ClipID == "" {
		_ = os.Remove(session.StagedPath)
		matches, _ := filepath.Glob(session.StagedPath + ".part.*")
		for _, m := range matches {
			_ = os.Remove(m)
		}
	}
	writeJSON(w, http.StatusOK, schemas.DeleteResponse{OK: true})
}

// POST /api/uploads/{id}/complete -- assemble (+ optionally create clip).
func (a *App) handleCompleteUpload(w http.ResponseWriter, r *http.Request) {
	uploadID := strings.TrimSpace(uploadIDFromRequest(r))
	if uploadID == "" {
		writeError(w, newHTTPError(http.StatusNotFound, "上传会话不存在"))
		return
	}
	body, err := readBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	clipParams, validationError := schemas.ParseUploadCompleteRequest(body)
	if validationError != nil {
		writeError(w, validationError)
		return
	}

	// Fast path: already completed -> idempotent replay (covers "response lost"
	// after a successful complete; client retry must not create a 2nd clip).
	if existing, _ := a.Repo.GetUploadSession(uploadID); existing != nil && existing.Status == repository.UploadStatusCompleted {
		a.replayCompletedUpload(w, r, existing)
		return
	}

	session, _, ok, _ := a.loadActiveUpload(w, uploadID)
	if !ok {
		return
	}
	if session.Status == repository.UploadStatusCompleted {
		// Raced with another completer between the fast-path check and load.
		if fresh, _ := a.Repo.GetUploadSession(uploadID); fresh != nil {
			a.replayCompletedUpload(w, r, fresh)
			return
		}
		writeError(w, newHTTPError(http.StatusConflict, "上传已完成"))
		return
	}
	if session.Status == repository.UploadStatusCompleting {
		writeError(w, newHTTPError(http.StatusConflict, "上传正在合并，请稍后查询"))
		return
	}

	// Clip-mode pre-validation BEFORE touching upload state or doing assembly
	// IO: token ownership (short DB) and field sanity. Failures here leave
	// chunks intact for retry with corrected params. Note: captcha was already
	// verified at init (before any disk was consumed), so it is NOT repeated
	// here — Turnstile tokens are single-use and would fail the second time.
	if clipParams.HasClipFields {
		if proceed := a.preValidateCompleteClip(w, r, session, clipParams); !proceed {
			return
		}
	}

	// Reserve the final path and flip active->completing atomically (short lock).
	// stagedPath is recorded for crash recovery: a power loss mid-assembly
	// leaves `completing` + stagedPath behind, which startup resets.
	_, finalPath := storage.FinalStoragePath(a.Settings.FileStorageDir, session.Filename, session.MimeType)
	completing, _, err := a.Repo.TryBeginComplete(uploadID, finalPath)
	if err != nil {
		a.writeCompleteTransitionError(w, err)
		return
	}

	// Assembly IO with NO locks held. Any failure rolls the session back to
	// active (FailComplete) and deletes the partial staged file, preserving
	// chunks for resume/retry.
	if err := a.assembleSessionFiles(session, completing); err != nil {
		var missing *repository.MissingChunksError
		if errors.As(err, &missing) {
			// Chunks vanished from disk (manual deletion / disk loss): prune DB
			// receipts so the next GET accurately reports them as missing.
			_ = a.Repo.DeleteChunkRecords(uploadID, missing.Missing)
		}
		if staged, _ := a.Repo.FailComplete(uploadID); staged != "" {
			_ = os.Remove(staged)
			matches, _ := filepath.Glob(staged + ".part.*")
			for _, m := range matches {
				_ = os.Remove(m)
			}
		} else {
			_ = os.Remove(finalPath)
		}
		// Surface missing-chunk details for resume.
		var missingErr *repository.MissingChunksError
		if errors.As(err, &missingErr) {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{
				"detail":  "分片缺失，请续传后重试",
				"missing": missingErr.Missing,
			})
			return
		}
		a.logger.Printf("ERROR:    unable to assemble upload %s: %v", uploadID, err)
		// Disk-loss of a single chunk file surfaces as "missing chunk N".
		if strings.HasPrefix(err.Error(), "missing chunk ") {
			writeError(w, newHTTPError(http.StatusBadRequest, "分片缺失，请续传后重试"))
			return
		}
		writeError(w, newHTTPError(http.StatusInternalServerError, "文件合并失败，请重试"))
		return
	}

	// Assembly succeeded. Clip mode: create the clip (short DB); on failure
	// delete the assembled file (rollback) and flip back to active.
	if clipParams.HasClipFields {
		clip, err := a.createClipFromCompletedUpload(session, completing, clipParams)
		if err != nil {
			// Rollback: staged file is orphan (no clip owns it) -> delete.
			_ = os.Remove(completing.StagedPath)
			if staged, _ := a.Repo.FailComplete(uploadID); staged != "" && staged != completing.StagedPath {
				_ = os.Remove(staged)
			}
			a.writeClipCreationError(w, err)
			return
		}
		completed, err := a.Repo.CompleteUploadSession(uploadID, clip.ID)
		if err != nil {
			// Extremely rare: session was aborted/expired between assembly and
			// commit. The clip already exists (do NOT delete user data); just
			// surface success via the clip (session row is gone, cannot record).
			a.logger.Printf("ERROR:    complete commit raced abort for %s (clip %s kept): %v", uploadID, clip.ID, err)
			writeJSON(w, http.StatusCreated, schemas.ClipFromModel(clip, utils.BuildBaseURL(r)))
			return
		}
		_ = storage.RemoveUploadDir(a.Settings.FileStorageDir, uploadID)
		_ = completed
		writeJSON(w, http.StatusCreated, schemas.ClipFromModel(clip, utils.BuildBaseURL(r)))
		return
	}

	// File-only mode: mark completed (staged file owned by the session until TTL).
	completed, err := a.Repo.CompleteUploadSession(uploadID, "")
	if err != nil {
		_ = os.Remove(completing.StagedPath)
		a.logger.Printf("ERROR:    unable to commit completed upload %s: %v", uploadID, err)
		writeError(w, newHTTPError(http.StatusInternalServerError, "文件合并失败，请重试"))
		return
	}
	_ = storage.RemoveUploadDir(a.Settings.FileStorageDir, uploadID)
	resp := schemas.UploadCompleteFileResponse{
		UploadID: completed.ID, Status: completed.Status, ExpiresAt: toMillis(completed.ExpiresAt),
	}
	resp.File.Name = completed.Filename
	resp.File.Size = completed.FileSize
	resp.File.Type = completed.MimeType
	writeJSON(w, http.StatusOK, resp)
}

// replayCompletedUpload renders the idempotent result of an already-completed
// session (retry after lost response).
func (a *App) replayCompletedUpload(w http.ResponseWriter, r *http.Request, session *repository.UploadSession) {
	if session.ClipID != "" {
		clip, err := a.Repo.GetClip(session.ClipID)
		if err != nil {
			writeError(w, err)
			return
		}
		if clip == nil {
			writeError(w, newHTTPError(http.StatusGone, "文件已过期或销毁"))
			return
		}
		writeJSON(w, http.StatusOK, schemas.ClipFromModel(clip, utils.BuildBaseURL(r)))
		return
	}
	resp := schemas.UploadCompleteFileResponse{
		UploadID: session.ID, Status: session.Status, ExpiresAt: toMillis(session.ExpiresAt),
	}
	resp.File.Name = session.Filename
	resp.File.Size = session.FileSize
	resp.File.Type = session.MimeType
	writeJSON(w, http.StatusOK, resp)
}

func (a *App) writeCompleteTransitionError(w http.ResponseWriter, err error) {
	var missing *repository.MissingChunksError
	if errors.As(err, &missing) {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"detail":  "分片缺失，请续传后重试",
			"missing": missing.Missing,
		})
		return
	}
	var expired *repository.SessionExpiredError
	if errors.As(err, &expired) {
		if expired.Session != nil {
			a.expireUploadNow(expired.Session)
		}
		writeError(w, newHTTPError(http.StatusGone, "上传会话已过期，请重新上传"))
		return
	}
	var completed *repository.SessionCompletedError
	if errors.As(err, &completed) {
		writeError(w, newHTTPError(http.StatusConflict, "上传已完成"))
		return
	}
	var completing *repository.SessionCompletingError
	if errors.As(err, &completing) {
		writeError(w, newHTTPError(http.StatusConflict, "上传正在合并，请稍后查询"))
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, newHTTPError(http.StatusNotFound, "上传会话不存在"))
		return
	}
	writeError(w, err)
}

// assembleSessionFiles streams chunks into the staged path and verifies size.
// No locks held. Returns MissingChunksError when a chunk file is absent.
func (a *App) assembleSessionFiles(session *repository.UploadSession, completing *repository.UploadSession) error {
	staged := completing.StagedPath
	if staged == "" {
		return errors.New("staged path missing")
	}
	// Pre-flight: every chunk file must exist (else report precise missing set).
	var missing []int
	for i := 0; i < session.TotalChunks; i++ {
		if _, err := os.Stat(storage.ChunkFilePath(a.Settings.FileStorageDir, session.ID, i)); err != nil {
			if os.IsNotExist(err) {
				missing = append(missing, i)
			} else {
				return fmt.Errorf("unable to stat chunk %d: %w", i, err)
			}
		}
	}
	if len(missing) > 0 {
		return &repository.MissingChunksError{Missing: missing}
	}
	if err := storage.AssembleChunks(a.Settings.FileStorageDir, session.ID, session.TotalChunks, staged, session.FileSize); err != nil {
		// Translate a mid-assembly disappearance into a resume hint.
		if strings.HasPrefix(err.Error(), "missing chunk ") {
			// Re-scan to build the accurate missing set.
			missing = missing[:0]
			for i := 0; i < session.TotalChunks; i++ {
				if _, statErr := os.Stat(storage.ChunkFilePath(a.Settings.FileStorageDir, session.ID, i)); statErr != nil {
					if os.IsNotExist(statErr) {
						missing = append(missing, i)
					}
				}
			}
			if len(missing) > 0 {
				return &repository.MissingChunksError{Missing: missing}
			}
		}
		return err
	}
	// Post-verify size on disk (defense in depth against disk corruption).
	info, err := os.Stat(staged)
	if err != nil {
		return fmt.Errorf("unable to verify assembled file: %w", err)
	}
	if info.Size() != session.FileSize {
		_ = os.Remove(staged)
		return fmt.Errorf("assembled size mismatch: got %d, want %d", info.Size(), session.FileSize)
	}
	return nil
}

// preValidateCompleteClip mirrors handleCreateClip's token gate (captcha gate
// moved to init). Runs before assembly so failures never burn a merge.
func (a *App) preValidateCompleteClip(w http.ResponseWriter, r *http.Request, session *repository.UploadSession, params *schemas.UploadCompleteClipParams) bool {
	envID := strings.TrimSpace(params.EnvironmentID)
	if envID == "" {
		writeError(w, newHTTPError(http.StatusBadRequest, "缺少 environmentId"))
		return false
	}
	if params.AccessToken != nil && *params.AccessToken != "" {
		if _, err := a.Repo.EnsureTokenOwner(*params.AccessToken, envID); err != nil {
			var valueError *apperr.ValueError
			if !errors.As(err, &valueError) {
				writeError(w, err)
				return false
			}
			message := valueError.Message
			if strings.Contains(message, "未注册") || strings.Contains(message, "未找到") {
				if _, registerErr := a.Repo.RegisterToken(*params.AccessToken, &envID); registerErr != nil {
					a.logger.Printf("ERROR:    unable to register token: %v", registerErr)
					http.Error(w, "Internal Server Error", http.StatusInternalServerError)
					return false
				}
			} else {
				writeError(w, newHTTPError(http.StatusConflict, message))
				return false
			}
		}
	}
	return true
}

// createClipFromCompletedUpload persists the clip row referencing the staged file.
func (a *App) createClipFromCompletedUpload(session *repository.UploadSession, completing *repository.UploadSession, params *schemas.UploadCompleteClipParams) (*models.Clip, error) {
	stored := &models.StoredFile{
		Name: session.Filename, Size: session.FileSize, Mime: session.MimeType, Path: completing.StagedPath,
	}
	if stored.Size > a.Settings.MaxFileSizeBytes {
		return nil, newHTTPError(http.StatusBadRequest, "文件体积超过限制")
	}
	envID := strings.TrimSpace(params.EnvironmentID)
	return a.Repo.CreateClip(repository.CreateClipParams{
		ClipType: models.ClipTypeFile, ExpiresAtMs: params.ExpiresAt,
		MaxDownloads: params.MaxDownloads, AccessCode: params.AccessCode,
		AccessToken: params.AccessToken, EnvironmentID: envID, StoredFile: stored,
	})
}

func (a *App) writeClipCreationError(w http.ResponseWriter, err error) {
	var httpErr *apperr.HTTPError
	if errors.As(err, &httpErr) {
		writeError(w, httpErr)
		return
	}
	var valueError *apperr.ValueError
	if errors.As(err, &valueError) {
		status := http.StatusBadRequest
		if strings.Contains(valueError.Message, "已存在") || strings.Contains(valueError.Message, "Token") {
			status = http.StatusConflict
		}
		writeError(w, newHTTPError(status, valueError.Message))
		return
	}
	writeError(w, err)
}

// ---------------------------------------------------------------------------
// Startup recovery + periodic maintenance (power-loss / timeout rollback).
// ---------------------------------------------------------------------------

// ReconcileUploadsOnStartup purges expired sessions, rolls back `completing`
// rows left by a crash, re-syncs DB receipts with files on disk, and sweeps
// orphan chunk dirs + orphan assembly temps. All DB work uses short locks;
// all file IO runs outside locks.
func (a *App) ReconcileUploadsOnStartup() {
	now := time.Now().UTC().Unix()
	// 1) Expired sessions (timeout rollback).
	if victims, err := a.Repo.PurgeExpiredUploads(now); err != nil {
		a.logger.Printf("ERROR:    upload expiry purge failed: %v", err)
	} else {
		for _, v := range victims {
			_ = storage.RemoveUploadDir(a.Settings.FileStorageDir, v.UploadID)
			if v.StagedPath != "" && !v.HasClip {
				_ = os.Remove(v.StagedPath)
				matches, _ := filepath.Glob(v.StagedPath + ".part.*")
				for _, m := range matches {
					_ = os.Remove(m)
				}
			}
		}
		if len(victims) > 0 {
			a.logger.Printf("INFO:     startup upload purge removed %d expired session(s)", len(victims))
		}
	}
	// 2) `completing` rows stuck by power loss -> back to active.
	if stuck, err := a.Repo.ResetStuckCompleting(); err != nil {
		a.logger.Printf("ERROR:    upload completing reset failed: %v", err)
	} else {
		for _, s := range stuck {
			_ = os.Remove(storage.AssembledTempPath(a.Settings.FileStorageDir, s.UploadID))
			if s.StagedPath != "" {
				_ = os.Remove(s.StagedPath)
				matches, _ := filepath.Glob(s.StagedPath + ".part.*")
				for _, m := range matches {
					_ = os.Remove(m)
				}
			}
		}
		if len(stuck) > 0 {
			a.logger.Printf("INFO:     startup upload recovery reset %d interrupted session(s) to active", len(stuck))
		}
	}
	// 3) Per-session disk<->DB re-sync (active sessions only).
	sessions, err := a.Repo.ListUploadSessions()
	if err != nil {
		a.logger.Printf("ERROR:    unable to list upload sessions for reconcile: %v", err)
	} else {
		for _, s := range sessions {
			if s.Status == repository.UploadStatusCompleted {
				// Chunks should already be gone; sweep leftovers (crash between
				// commit and dir removal).
				_ = storage.RemoveUploadDir(a.Settings.FileStorageDir, s.ID)
				continue
			}
			received, err := a.Repo.ListReceivedChunks(s.ID)
			if err != nil {
				continue
			}
			_ = a.reconcileDiskState(s, received)
			// Drop stray assembled temps (crash before ResetStuckCompleting?).
			_ = os.Remove(storage.AssembledTempPath(a.Settings.FileStorageDir, s.ID))
		}
	}
	// 4) Orphan chunk dirs (PUT raced DELETE, or manual DB loss).
	if dirs, err := storage.ListUploadDirsOnDisk(a.Settings.FileStorageDir); err != nil {
		a.logger.Printf("ERROR:    unable to list upload dirs: %v", err)
	} else {
		for _, id := range dirs {
			exists, err := a.Repo.UploadSessionExists(id)
			if err != nil || exists {
				continue
			}
			_ = storage.RemoveUploadDir(a.Settings.FileStorageDir, id)
			a.logger.Printf("INFO:     removed orphan upload dir %s", id)
		}
	}
	// 5) Orphan assembly temps in the file root (*.part.* with no session ref).
	// Only on startup (no in-flight assemblies), so deletion is safe.
	if entries, err := os.ReadDir(a.Settings.FileStorageDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if strings.Contains(e.Name(), ".part.") {
				_ = os.Remove(filepath.Join(a.Settings.FileStorageDir, e.Name()))
			}
		}
	}
	// 6) Upload quota ledger: an upgrade from a pre-quota database (or a crash
	// between "reserve" and "insert") may leave the counter drifting.
	a.reconcileUploadQuota()
}

// reconcileUploadQuota recomputes the reserved bytes from live sessions and
// overwrites the ledger, then logs the result. It only fixes the counter --
// no chunk dir and no clip file is touched.
func (a *App) reconcileUploadQuota() {
	recomputed, previous, err := a.Repo.RecomputeUploadQuota()
	if err != nil {
		a.logger.Printf("ERROR:    upload quota reconciliation failed: %v", err)
		return
	}
	if recomputed != previous {
		a.logger.Printf("WARN:     upload quota drift corrected: reserved_bytes %d -> %d (SUM(file_size) of active unreleased sessions)",
			previous, recomputed)
		return
	}
	a.logger.Printf("INFO:     upload quota reconciled: reserved_bytes = %d (no drift)", recomputed)
}

// purgeExpiredUploadsPeriodic is called by the cleanup worker (timeout path).
func (a *App) purgeExpiredUploadsPeriodic() {
	victims, err := a.Repo.PurgeExpiredUploads(time.Now().UTC().Unix())
	if err != nil {
		a.logger.Printf("ERROR:    upload cleanup worker failed: %v", err)
		return
	}
	for _, v := range victims {
		_ = storage.RemoveUploadDir(a.Settings.FileStorageDir, v.UploadID)
		if v.StagedPath != "" && !v.HasClip {
			_ = os.Remove(v.StagedPath)
			matches, _ := filepath.Glob(v.StagedPath + ".part.*")
			for _, m := range matches {
				_ = os.Remove(m)
			}
		}
	}
}
