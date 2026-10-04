package api

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
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
//   - ONE file per upload, pre-allocated by INIT at its FINAL path with
//     truncate(fileSize) (sparse: no bytes are consumed until written). Because
//     fileSize and chunkSize are known at init, every chunk's [start, end) byte
//     range is known too, so PUT writes its bytes directly into that range with
//     WriteAt and COMPLETE has nothing to merge -- no temp copy, no double disk
//     usage, no blocking assembly step.
//   - A chunk is "received" exactly when its receipt row is committed in
//     upload_chunks; the database is the only source of truth for progress. The
//     receipt is written AFTER the bytes were written and fsynced, so a crash can
//     only under-report progress (the client re-sends that range), never
//     over-report it.
//   - Every chunk but the last one must carry exactly chunkSize bytes (the last
//     one carries the remainder); any other payload size is refused with a
//     parameter error before/while writing, so a range can never be half-filled
//     and then marked complete.
//   - No global lock is held during file IO (see repository docs), and chunk
//     ranges are disjoint, so parallel PUTs of one upload are safe.
// ---------------------------------------------------------------------------

func uploadIDFromRequest(r *http.Request) string {
	// Route registers {id}; be tolerant to {upload_id} naming too.
	if v := routeParam(r, "id"); v != "" {
		return v
	}
	return routeParam(r, "upload_id")
}

// expireUploadNow deletes an expired session (DB under short lock, file outside)
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
	// An aborted session never owns a clip, so its pre-allocated file is orphan.
	if aborted.StagedPath != "" && aborted.ClipID == "" {
		_ = storage.RemoveUploadFile(aborted.StagedPath)
	}
}

// loadActiveUpload loads the session, handling 404/410 mapping.
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
	// Expiry only ends the *resume* window. A completed session is a pure
	// idempotency record (see UploadSession.AcceptsReplay): its row may outlive
	// expires_at until the cleanup worker drops it, and in that window a retry
	// must still get the original answer instead of a 410 that would make the
	// client upload the file again.
	if !s.AcceptsReplay(time.Now().UTC().Unix()) {
		a.expireUploadNow(s)
		writeError(w, newHTTPError(http.StatusGone, "上传会话已过期，请重新上传"))
		return nil, nil, false, true
	}
	return s, r, true, false
}

// ensureUploadStorage guarantees the session's byte container exists with the
// exact declared length. It reports recreated=true when the file had to be
// created/resized, which invalidates every receipt: the caller must drop them
// because the byte ranges they describe are gone (the DB is the single source of
// truth, so a stale receipt would be read as "this range is on disk").
func (a *App) ensureUploadStorage(session *repository.UploadSession) (recreated bool, err error) {
	if session == nil || session.StagedPath == "" {
		return false, errors.New("upload session has no storage path")
	}
	return storage.EnsureFileSize(session.StagedPath, session.FileSize)
}

// allChunkIndices returns [0, totalChunks) as a list (used when the whole upload
// has to be re-sent).
func allChunkIndices(total int) []int {
	if total <= 0 {
		return []int{}
	}
	out := make([]int, 0, total)
	for i := 0; i < total; i++ {
		out = append(out, i)
	}
	return out
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

// preallocateUploadFile materialises the session's byte container at its final
// path and length. Tests replace App.preallocateUploadFn to exercise the failure
// path (row committed, container missing).
func (a *App) preallocateUploadFile(path string, size int64) error {
	if a.preallocateUploadFn != nil {
		return a.preallocateUploadFn(path, size)
	}
	return storage.PreallocateFile(path, size)
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
//
// The byte container is created here, at its final path: truncate(fileSize)
// fixes the total length while the file is already in place, so later PUTs write
// in-range and COMPLETE only has to insert the clip row.
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
	// of init never allocates a second file or leaks a session row.
	if req.RequestID != "" {
		existing, err := a.Repo.GetUploadSessionByRequestID(req.EnvironmentID, req.RequestID)
		if err != nil {
			writeError(w, err)
			return
		}
		if existing != nil {
			// Ignore expires_at for a finished session: it is replayable until
			// its row is actually purged, so a retried init can never open a
			// second session (and a second clip) for the same requestId.
			if existing.AcceptsReplay(time.Now().UTC().Unix()) {
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
			// Expired key: purge row AND file so a fresh session can reuse the
			// idempotency slot without orphaning old bytes.
			a.expireUploadNow(existing)
		}
	}

	// Clip-param business rules (format already enforced by ParseUploadInitRequest)
	// run before captcha/quota so a bad expiresAt never consumes storage.
	// Every upload becomes a clip, so they always apply; idempotent replays above
	// skip them so a live session is returned as-is.
	if !time.UnixMilli(req.ExpiresAt).UTC().After(time.Now().UTC()) {
		writeError(w, newHTTPError(http.StatusBadRequest, "过期时间必须晚于当前时间"))
		return
	}

	// Captcha gate: verified once, up-front, before a single byte can be stored.
	// Turnstile tokens are single-use & short-lived, so they belong at init.
	if !a.verifyCaptcha(w, r, req.CaptchaToken, req.CaptchaProvider) {
		return
	}

	// Clip business rules (token ownership, access-code uniqueness against
	// already-inserted clips) run after captcha and BEFORE quota reservation
	// so a doomed init never consumes storage. Concurrent inits with the same
	// unused code can still race at complete; that path rolls back.
	if req.AccessToken != nil && *req.AccessToken != "" {
		if err := a.ensureAccessTokenOwner(*req.AccessToken, req.EnvironmentID); err != nil {
			writeError(w, err)
			return
		}
	}
	if req.AccessCode != nil && *req.AccessCode != "" {
		existing, err := a.Repo.GetClipByCode(*req.AccessCode)
		if err != nil {
			writeError(w, err)
			return
		}
		if existing != nil {
			writeError(w, newHTTPError(http.StatusConflict, "直链码已存在，请刷新后再试"))
			return
		}
	}

	// ---- Global storage gates. Fixed order: live-session cap, then quota
	// reservation (atomic CAS in the DB), then the free-disk watermark. All of
	// them run before the session row exists, so a refusal leaves nothing
	// behind: no session, no file, no dangling reservation.
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
	// The byte container path is decided BEFORE the insert: it is the final clip
	// path, so the row and the file are born together and never need a rename.
	_, finalPath := storage.FinalStoragePath(a.Settings.FileStorageDir, req.Filename, req.MimeType)
	session, created, err := a.Repo.CreateOrGetUploadSession(repository.CreateUploadSessionParams{
		Filename: req.Filename, FileSize: req.FileSize, MimeType: req.MimeType,
		EnvironmentID: req.EnvironmentID, RequestID: req.RequestID,
		ChunkSize: chunkSize, TTLSeconds: ttl,
		ClipExpiresAt:    req.ExpiresAt,
		ClipMaxDownloads: req.MaxDownloads, ClipAccessCode: req.AccessCode,
		ClipAccessToken: req.AccessToken,
		StagedPath:      finalPath,
	})
	if err != nil {
		releaseReservation()
		// The idempotency key is still held by a session we cannot purge yet
		// (expired mid-complete): surface a retryable conflict instead of a 200
		// carrying an uploadId that can only fail.
		var unavailable *repository.SessionUnavailableError
		if errors.As(err, &unavailable) {
			writeError(w, newHTTPError(http.StatusConflict, "上一个上传会话仍在处理中，请稍后重试"))
			return
		}
		writeError(w, err)
		return
	}
	if created {
		// The session row owns the reservation from here on; abort, expiry
		// cleanup and complete success all return it through the idempotent
		// quota_released CAS. A failed complete rolls back to `active` and keeps
		// both its bytes and its reservation (terminal transitions only).
		reservationHeld = false
	}
	if !created {
		// Lost the insert race or an exact-replica row appeared in between: the
		// winning session already holds a reservation and owns its file, so give
		// ours back (a replayed init must never reserve twice) and never allocate.
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
	// File creation is IO outside the repository lock. On failure roll back the
	// DB row (which also returns the reservation) so no orphan session remains.
	if err := a.preallocateUploadFile(finalPath, session.FileSize); err != nil {
		// The row exists, so the reservation is already owned by a session row:
		// abort returns it through the quota_released CAS (the deferred
		// releaseReservation() above must NOT run -- it would decrement twice).
		if _, abortErr := a.Repo.AbortUploadSession(session.ID); abortErr != nil {
			a.logger.Printf("ERROR:    unable to roll back session %s after pre-allocation failure: %v", session.ID, abortErr)
		}
		_ = storage.RemoveUploadFile(finalPath)
		a.logger.Printf("ERROR:    unable to pre-allocate upload file %s: %v", finalPath, err)
		writeError(w, newHTTPError(http.StatusInternalServerError, "无法创建上传文件，请重试"))
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
//
// The payload is written straight into its byte range of the pre-allocated file
// (offset = index * chunkSize, length = expected chunk size). Sizes are enforced
// twice: a Content-Length that disagrees with the range is refused before any
// IO, and a body that delivers a different length is refused while streaming.
// Only a fully written, fsynced range is recorded in the database.
func (a *App) handlePutChunk(w http.ResponseWriter, r *http.Request) {
	uploadID := uploadIDFromRequest(r)
	indexRaw := routeParam(r, "index")
	index, err := strconv.Atoi(strings.TrimSpace(indexRaw))
	if err != nil || index < 0 {
		writeError(w, newHTTPError(http.StatusBadRequest, "分片序号无效"))
		return
	}
	session, _, ok, _ := a.loadActiveUpload(w, uploadID)
	if !ok {
		return
	}
	if session.Status == repository.UploadStatusCompleted {
		writeError(w, newHTTPError(http.StatusConflict, "上传已完成"))
		return
	}
	if session.Status == repository.UploadStatusCompleting {
		writeError(w, newHTTPError(http.StatusConflict, "上传正在完成，请稍后重试"))
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
	if session.StagedPath == "" {
		// Only rows written by an older (pre-allocated) version: they have no
		// container, so they cannot be resumed -- ask for a fresh upload.
		writeError(w, newHTTPError(http.StatusBadRequest, "上传会话缺少文件路径，请重新上传"))
		return
	}
	expected, _ := session.ExpectedChunkSize(index)
	offset, _ := session.ChunkOffset(index)
	// Size contract: every chunk but the last one is exactly chunkSize bytes,
	// the last one is the remainder. A declared length that disagrees is a
	// parameter error -> refuse before touching the file.
	if r.ContentLength > expected {
		writeError(w, newHTTPError(http.StatusRequestEntityTooLarge, "分片体积超过限制"))
		return
	}
	if r.ContentLength >= 0 && r.ContentLength != expected {
		writeError(w, newHTTPError(http.StatusBadRequest,
			fmt.Sprintf("分片大小不匹配：第 %d 块应为 %d 字节，实际声明 %d 字节", index, expected, r.ContentLength)))
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
	// Self-heal a lost/resized container: recreate the file and drop receipts,
	// because the ranges they describe no longer exist.
	recreated, err := a.ensureUploadStorage(session)
	if err != nil {
		a.logger.Printf("ERROR:    unable to prepare upload file %s: %v", session.StagedPath, err)
		writeError(w, newHTTPError(http.StatusInternalServerError, "分片存储失败，请重试"))
		return
	}
	if recreated {
		if fresh, _ := a.Repo.GetUploadSession(session.ID); fresh == nil {
			_ = storage.RemoveUploadFile(session.StagedPath)
			writeError(w, newHTTPError(http.StatusNotFound, "上传会话不存在"))
			return
		}
		if err := a.Repo.ClearChunkReceipts(session.ID); err != nil {
			a.logger.Printf("ERROR:    unable to reset chunk receipts for %s: %v", session.ID, err)
			writeError(w, err)
			return
		}
		a.logger.Printf("WARN:    upload file %s was missing/unsized; re-created and all receipts cleared", session.StagedPath)
	}
	// Stream into the byte range (no global lock, ranges are disjoint). Client
	// cancellation surfaces as a read error: the range stays unrecorded, so the
	// session remains resumable.
	defer r.Body.Close()
	written, err := storage.WriteChunkRange(session.StagedPath, offset, expected, r.Body)
	if err != nil {
		if errors.Is(err, storage.ErrChunkTooLarge) {
			writeError(w, newHTTPError(http.StatusRequestEntityTooLarge, "分片体积超过限制"))
			return
		}
		if r.Context().Err() != nil {
			// Client went away; nothing to render (connection dead).
			return
		}
		a.logger.Printf("ERROR:    unable to store chunk %s/%d: %v", session.ID, index, err)
		writeError(w, newHTTPError(http.StatusInternalServerError, "分片存储失败，请重试"))
		return
	}
	if written != expected {
		// Truncated body: the range is incomplete, so it must NOT be recorded.
		// Any older receipt for this index is dropped as well, because this
		// attempt may have overwritten part of a previously good range.
		if err := a.Repo.DeleteChunkReceipt(session.ID, index); err != nil {
			a.logger.Printf("ERROR:    unable to drop partial receipt %s/%d: %v", session.ID, index, err)
		}
		writeError(w, newHTTPError(http.StatusBadRequest,
			fmt.Sprintf("分片大小不匹配：第 %d 块应为 %d 字节，实际收到 %d 字节，请重传该分片", index, expected, written)))
		return
	}
	// The range is complete: make it durable, then publish the receipt.
	if err := storage.FlushFile(session.StagedPath); err != nil {
		a.logger.Printf("ERROR:    unable to flush upload file %s: %v", session.StagedPath, err)
		writeError(w, newHTTPError(http.StatusInternalServerError, "分片存储失败，请重试"))
		return
	}
	if err := a.Repo.MarkChunkReceived(session.ID, index, written); err != nil {
		// The chunk cannot be recorded honestly: undo the range and let the
		// client retry instead of reporting a success the DB does not know.
		if err := a.Repo.DeleteChunkReceipt(session.ID, index); err != nil {
			a.logger.Printf("ERROR:    unable to drop unrecorded receipt %s/%d: %v", session.ID, index, err)
		}
		if errors.Is(err, sql.ErrNoRows) {
			if recreated {
				// The container was created by this request and nothing owns it.
				_ = storage.RemoveUploadFile(session.StagedPath)
			}
			writeError(w, newHTTPError(http.StatusNotFound, "上传会话不存在"))
			return
		}
		if errors.Is(err, repository.ErrUploadAlreadyCompleted) {
			writeError(w, newHTTPError(http.StatusConflict, "上传已完成"))
			return
		}
		var mismatch *repository.ChunkSizeMismatchError
		if errors.As(err, &mismatch) {
			writeError(w, newHTTPError(http.StatusBadRequest, "分片大小不匹配，请重传该分片"))
			return
		}
		a.logger.Printf("ERROR:    unable to record chunk %s/%d: %v", session.ID, index, err)
		writeError(w, err)
		return
	}
	// Re-read for an accurate progress response (cheap indexed read).
	updated, err := a.Repo.ListReceivedChunks(session.ID)
	if err != nil {
		a.logger.Printf("ERROR:    unable to list received chunks for %s: %v", session.ID, err)
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, schemas.UploadChunkResponse{
		UploadID: session.ID, Index: index, ReceivedCount: len(updated),
		TotalChunks: session.TotalChunks, ReceivedChunks: updated,
		Complete: len(updated) >= session.TotalChunks,
	})
}

// GET /api/uploads/{id} -- resume info. The response is computed from the
// database alone: upload_chunks is the single source of truth for progress.
func (a *App) handleGetUpload(w http.ResponseWriter, r *http.Request) {
	uploadID := uploadIDFromRequest(r)
	session, received, ok, _ := a.loadActiveUpload(w, uploadID)
	if !ok {
		return
	}
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
			writeError(w, newHTTPError(http.StatusConflict, "上传正在完成，无法取消"))
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
	// File deletion happens outside the lock. A completed session (which always
	// owns a clip) is rejected above, so the pre-allocated file belongs to an
	// aborted upload and is always safe to delete.
	if session.StagedPath != "" && session.ClipID == "" {
		_ = storage.RemoveUploadFile(session.StagedPath)
	}
	writeJSON(w, http.StatusOK, schemas.DeleteResponse{OK: true})
}

// POST /api/uploads/{id}/complete -- turn a finished upload into a clip.
//
// There is NO assembly step anymore: INIT already created the final file at its
// final path and every PUT wrote its bytes in place, so COMPLETE only verifies
// the receipts, flips the state machine (active->completing->completed) and
// inserts the clip row pointing at the file. That keeps a large upload from
// blocking the API for the duration of a merge and removes the second full copy
// of the data from disk.
//
// The clip parameters are NOT accepted here: they were validated at init and
// frozen on the session row, so a client cannot bypass validation by sending
// different (or missing) params at complete time, and a retried complete cannot
// create a clip with different access rules than the session was booked with.
// There is no file-only mode: every session ends in a clip row.
func (a *App) handleCompleteUpload(w http.ResponseWriter, r *http.Request) {
	uploadID := strings.TrimSpace(uploadIDFromRequest(r))
	if uploadID == "" {
		writeError(w, newHTTPError(http.StatusNotFound, "上传会话不存在"))
		return
	}
	// Drain any leftover body: complete carries no parameters (older clients may
	// still send a JSON object; it is ignored, never trusted).
	if _, err := readBody(r); err != nil {
		writeError(w, err)
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
		writeError(w, newHTTPError(http.StatusConflict, "上传正在完成，请稍后查询"))
		return
	}
	if session.StagedPath == "" {
		writeError(w, newHTTPError(http.StatusBadRequest, "上传会话缺少文件路径，请重新上传"))
		return
	}

	// Pre-validation of the session-frozen clip params BEFORE any state change:
	// environment + token ownership (short DB reads). Failures here leave the
	// receipts intact and resumable. Captcha and the clip field format were
	// already verified at init.
	if proceed := a.preValidateCompleteClip(w, r, session); !proceed {
		return
	}

	// Pre-flight the byte container. If it vanished (or was resized out of band)
	// every receipt became a lie: re-create the file, drop the receipts and tell
	// the client to re-send the whole upload.
	recreated, err := a.ensureUploadStorage(session)
	if err != nil {
		a.logger.Printf("ERROR:    unable to verify upload file %s: %v", session.StagedPath, err)
		writeError(w, newHTTPError(http.StatusInternalServerError, "文件校验失败，请重试"))
		return
	}
	if recreated {
		// DELETE/expiry remove the row BEFORE the file, so a missing file with a
		// live row means real disk loss; a missing row means the session is gone.
		if fresh, _ := a.Repo.GetUploadSession(uploadID); fresh == nil {
			_ = storage.RemoveUploadFile(session.StagedPath)
			writeError(w, newHTTPError(http.StatusNotFound, "上传会话不存在"))
			return
		}
		if err := a.Repo.ClearChunkReceipts(session.ID); err != nil {
			a.logger.Printf("ERROR:    unable to reset chunk receipts for %s: %v", session.ID, err)
			writeError(w, err)
			return
		}
		a.logger.Printf("WARN:    upload file %s was missing/unsized; all receipts cleared for %s", session.StagedPath, session.ID)
		writeJSON(w, http.StatusConflict, map[string]interface{}{
			"detail":  "上传文件已丢失，请重新上传全部分片",
			"missing": allChunkIndices(session.TotalChunks),
		})
		return
	}

	// Flip active->completing (short lock, pure concurrency guard: there is no
	// long IO to protect anymore) and verify every chunk has a receipt.
	completing, _, err := a.Repo.TryBeginComplete(uploadID)
	if err != nil {
		a.writeCompleteTransitionError(w, err)
		return
	}

	// The ONLY way to finish an upload is to insert the clip from the params
	// frozen at init. Concurrent completes can still collide (an accessCode
	// claimed since init, a token owner changed, the expiry that passed while
	// uploading): such a failure rolls the session back to active -- receipts and
	// bytes untouched, so the client can retry or abort -- and the typed error
	// (409 conflict / 400 value error) is surfaced.
	clip, err := a.createClipFromCompletedUpload(session, completing)
	if err != nil {
		if _, failErr := a.Repo.FailComplete(uploadID); failErr != nil {
			a.logger.Printf("ERROR:    unable to roll back complete for %s: %v", uploadID, failErr)
		}
		a.writeClipCreationError(w, err)
		return
	}
	completed, err := a.Repo.CompleteUploadSession(uploadID, clip.ID)
	if err != nil {
		// The clip already exists (do NOT delete user data); just surface success
		// via the clip (the session row is gone, so the link cannot be recorded).
		a.logger.Printf("ERROR:    complete commit raced abort for %s (clip %s kept): %v", uploadID, clip.ID, err)
		writeJSON(w, http.StatusCreated, schemas.ClipFromModel(clip, utils.BuildBaseURL(r)))
		return
	}
	_ = completed
	writeJSON(w, http.StatusCreated, schemas.ClipFromModel(clip, utils.BuildBaseURL(r)))
}

// replayCompletedUpload renders the idempotent result of an already-completed
// session (retry after lost response).
func (a *App) replayCompletedUpload(w http.ResponseWriter, r *http.Request, session *repository.UploadSession) {
	// A completed session always links its clip; only rows written before the
	// clip-only model can miss it, and those cannot be replayed as a clip.
	if session.ClipID == "" {
		writeError(w, newHTTPError(http.StatusGone, "上传记录已失效，请重新上传"))
		return
	}
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
		writeError(w, newHTTPError(http.StatusConflict, "上传正在完成，请稍后查询"))
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, newHTTPError(http.StatusNotFound, "上传会话不存在"))
		return
	}
	writeError(w, err)
}

// preValidateCompleteClip re-checks the session-frozen clip params (owner,
// expiry, token ownership) before any state change, so a doomed complete never
// flips the session.
func (a *App) preValidateCompleteClip(w http.ResponseWriter, r *http.Request, session *repository.UploadSession) bool {
	// init requires environmentId, so these two guards only fire for rows
	// written by a pre-upgrade version. Such a session has no owner and no clip
	// params, i.e. it can never become a (reachable) clip -> ask for a re-upload
	// instead of silently producing an unreachable file.
	envID := strings.TrimSpace(session.EnvironmentID)
	if envID == "" {
		writeError(w, newHTTPError(http.StatusBadRequest, "上传会话缺少 environmentId，请重新上传"))
		return false
	}
	if session.ClipExpiresAt <= 0 {
		writeError(w, newHTTPError(http.StatusBadRequest, "上传会话缺少片段参数，请重新上传"))
		return false
	}
	if session.ClipAccessToken != nil && *session.ClipAccessToken != "" {
		if err := a.ensureAccessTokenOwner(*session.ClipAccessToken, envID); err != nil {
			writeError(w, err)
			return false
		}
	}
	return true
}

// createClipFromCompletedUpload inserts the file clip against the session's
// pre-allocated file, using the params captured on the session at init. There is
// no "file-only" alternative: this is the only success path of COMPLETE. The file
// is already at its final path, so no move/copy happens here.
//
// It re-checks the invariants that a concurrent upload can break while this
// session was streaming chunks:
//   - a token that was re-registered to another environment -> token occupied;
//   - an accessCode that another completer took  -> 409 conflict (handled
//     atomically by CreateClip through the UNIQUE index on clips.access_code);
//   - an expiry that elapsed during the upload   -> value error (400).
//
// Callers must roll the session back (FailComplete) on error; the file and the
// receipts stay valid for a retry.
func (a *App) createClipFromCompletedUpload(session *repository.UploadSession, completing *repository.UploadSession) (*models.Clip, error) {
	stored := &models.StoredFile{
		Name: session.Filename, Size: session.FileSize, Mime: session.MimeType, Path: completing.StagedPath,
	}
	if stored.Size > a.Settings.MaxFileSizeBytes {
		return nil, newHTTPError(http.StatusBadRequest, "文件体积超过限制")
	}
	envID := strings.TrimSpace(session.EnvironmentID)
	if envID == "" {
		return nil, newHTTPError(http.StatusBadRequest, "上传会话缺少 environmentId，请重新上传")
	}
	return a.Repo.CreateClip(repository.CreateClipParams{
		ClipType: models.ClipTypeFile, ExpiresAtMs: session.ClipExpiresAt,
		MaxDownloads: session.ClipMaxDownloads, AccessCode: session.ClipAccessCode,
		AccessToken: session.ClipAccessToken, EnvironmentID: envID, StoredFile: stored,
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
		if valueError.Code == apperr.CodeAccessCodeConflict || valueError.Code == apperr.CodeTokenOccupied {
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
// rows left by a crash, sweeps orphan storage files (and leftovers of the old
// per-chunk layout). All DB work uses short locks; all file IO runs outside
// locks. Progress itself needs no reconciliation: upload_chunks is authoritative.
func (a *App) ReconcileUploadsOnStartup() {
	now := time.Now().UTC().Unix()
	// 1) Expired sessions (timeout rollback).
	if victims, err := a.Repo.PurgeExpiredUploads(now); err != nil {
		a.logger.Printf("ERROR:    upload expiry purge failed: %v", err)
	} else {
		a.deleteUploadVictimFiles(victims)
		if len(victims) > 0 {
			a.logger.Printf("INFO:     startup upload purge removed %d expired session(s)", len(victims))
		}
	}
	// 2) `completing` rows stuck by power loss -> back to active. Their file and
	// receipts are kept (they are exactly what a retried COMPLETE needs).
	if stuck, err := a.Repo.ResetStuckCompleting(); err != nil {
		a.logger.Printf("ERROR:    upload completing reset failed: %v", err)
	} else if len(stuck) > 0 {
		a.logger.Printf("INFO:     startup upload recovery reset %d interrupted session(s) to active", len(stuck))
	}
	// 3) Orphan storage files: everything that neither a live session nor a clip
	// references (a crash between INSERT and file creation, or manual leftovers).
	// Legacy per-chunk layout from the previous version is removed as a whole.
	a.sweepOrphanUploadFiles()
	// 4) Upload quota ledger: an upgrade from a pre-quota database (or a crash
	// between "reserve" and "insert") may leave the counter drifting.
	a.reconcileUploadQuota()
	// 5) Stored-clip ledger: an upgrade from a pre-quota database (or a crash
	// between "charge" and "commit") may leave the saved-bytes counter drifting.
	a.reconcileClipQuota()
}

// sweepOrphanUploadFiles deletes storage files no row references, plus any
// leftovers of the legacy chunk layout (<dir>/uploads/<id>/chunk-XXXXXX and
// *.part.* temps). It never touches a file that a session or a clip owns, so
// clip data is safe even if a session row was purged.
func (a *App) sweepOrphanUploadFiles() {
	referenced, err := a.Repo.ReferencedFilePaths()
	if err != nil {
		a.logger.Printf("ERROR:    unable to list referenced storage paths: %v", err)
		return
	}
	names, err := storage.ListStorageFiles(a.Settings.FileStorageDir)
	if err != nil {
		a.logger.Printf("ERROR:    unable to list storage dir: %v", err)
		return
	}
	removed := 0
	for _, name := range names {
		if strings.Contains(name, ".part.") {
			// Legacy assembly temp: always orphan (no code path writes them).
			if err := storage.RemoveUploadFile(filepath.Join(a.Settings.FileStorageDir, name)); err == nil {
				removed++
			}
			continue
		}
		path := filepath.Join(a.Settings.FileStorageDir, name)
		if referenced[path] {
			continue
		}
		if err := storage.RemoveUploadFile(path); err == nil {
			removed++
		}
	}
	if removed > 0 {
		a.logger.Printf("INFO:     startup sweep removed %d orphan storage file(s)", removed)
	}
	// Legacy per-session chunk directories are useless to this version (a legacy
	// session has no pre-allocated file and cannot be completed): drop them so an
	// upgrade does not keep the old bytes forever.
	legacyDir := storage.LegacyUploadRoot(a.Settings.FileStorageDir)
	if entries, err := os.ReadDir(legacyDir); err == nil && len(entries) > 0 {
		if err := os.RemoveAll(legacyDir); err != nil {
			a.logger.Printf("WARN:     unable to remove legacy chunk dir %s: %v", legacyDir, err)
		} else {
			a.logger.Printf("INFO:     removed legacy chunk layout (%d dirs) under %s", len(entries), legacyDir)
		}
	}
}

// deleteUploadVictimFiles removes the pre-allocated file of every purged session
// that never produced a clip.
func (a *App) deleteUploadVictimFiles(victims []repository.UploadPurgeVictim) {
	for _, v := range victims {
		if v.StagedPath != "" && !v.HasClip {
			_ = storage.RemoveUploadFile(v.StagedPath)
		}
	}
}

// reconcileUploadQuota recomputes the reserved bytes from live sessions and
// overwrites the ledger, then logs the result. It only fixes the counter -- no
// upload file and no clip file is touched.
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

// reconcileClipQuota recomputes the saved-clip bytes from the clips table and
// overwrites the ledger, then logs the result. It only fixes the counter -- no
// clip and no file is touched.
//
// Charge and release always ride along with the clip INSERT/DELETE transaction,
// so this is a belt-and-braces repair for drift left by an upgrade or a crash;
// it can never grant credit that the clips table does not back.
func (a *App) reconcileClipQuota() {
	recomputed, previous, err := a.Repo.RecomputeClipQuota()
	if err != nil {
		a.logger.Printf("ERROR:    clip storage quota reconciliation failed: %v", err)
		return
	}
	if recomputed != previous {
		a.logger.Printf("WARN:     clip storage quota drift corrected: used_bytes %d -> %d (SUM(file_size) of stored clips)",
			previous, recomputed)
		return
	}
	a.logger.Printf("INFO:     clip storage quota reconciled: used_bytes = %d (no drift)", recomputed)
}

// purgeExpiredUploadsPeriodic is called by the cleanup worker (timeout path).
func (a *App) purgeExpiredUploadsPeriodic() {
	victims, err := a.Repo.PurgeExpiredUploads(time.Now().UTC().Unix())
	if err != nil {
		a.logger.Printf("ERROR:    upload cleanup worker failed: %v", err)
		return
	}
	a.deleteUploadVictimFiles(victims)
	// Reap containers that no session row and no clip references any more. This
	// closes the last orphan path: an expired idempotency key purged inside the
	// repository (init racing the worker) returns no victim to the handler, so
	// nobody would delete the old container until the next process start.
	// A live upload can never be reaped: its row is inserted BEFORE its file is
	// created, so an in-flight container is always referenced.
	a.sweepOrphanUploadFiles()
}
