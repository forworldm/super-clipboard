package api

// API-level regression tests for the upload-quota invariants that the handlers
// are responsible for. They complement internal/repository/quota_consistency_test.go:
// here the state machine is driven through the real HTTP surface (init, PUT,
// COMPLETE) and the ledger/counters are then read back from the repository.

import (
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/pixia1234/super-clipboard/backend/internal/models"
	"github.com/pixia1234/super-clipboard/backend/internal/repository"
	"github.com/pixia1234/super-clipboard/backend/internal/storage"
)

// TestSessionCapCountsCompletingSessions: the cap must be evaluated against the
// sessions that still hold a reservation (quota_released = 0), not against
// `status = 'active'`. A session flipped to `completing` still owns its reserved
// bytes, so it must keep occupying the slot it was admitted with -- otherwise
// the cap could be exceeded by a session that is mid-COMPLETE.
func TestSessionCapCountsCompletingSessions(t *testing.T) {
	app := newTestApp(t, quotaSettings(0, 1, 0)) // no byte budget, ONE session slot

	// The single slot is taken by a live upload.
	requireStatus(t, initRaw(t, app, "cap-1.bin", 4096, "env-cap", ""), http.StatusCreated)
	uploadID := firstUploadID(t, app)
	requireStatus(t, putChunk(t, app, uploadID, 0, deterministicBytes(4096)), http.StatusOK)

	rec := initRaw(t, app, "cap-3.bin", 4096, "env-cap", "")
	requireStatus(t, rec, http.StatusInsufficientStorage)
	requireDetail(t, rec, "活动上传会话数已达上限")

	// Flip the session to `completing` behind the HTTP layer: its reservation is
	// still held and its slot must stay taken (only the terminal transition
	// releases), so the cap keeps refusing.
	if _, _, err := app.Repo.TryBeginComplete(uploadID); err != nil {
		t.Fatalf("begin complete: %v", err)
	}
	if reserved := reservedValue(t, app); reserved != 4096 {
		t.Fatalf("reserved = %d while completing, want 4096", reserved)
	}
	// The two counters deliberately disagree here: `active` is 0 (diagnostic),
	// the reservation holder count is 1 (what the cap and the ledger use).
	if active, err := app.Repo.CountActiveUploadSessions(); err != nil || active != 0 {
		t.Fatalf("active sessions = %d (err %v), want 0: the row is completing, not active", active, err)
	}
	if holders, err := app.Repo.CountUploadSessionsHoldingQuota(); err != nil || holders != 1 {
		t.Fatalf("reservation holders = %d (err %v), want 1", holders, err)
	}
	rec = initRaw(t, app, "cap-3.bin", 4096, "env-cap", "")
	requireStatus(t, rec, http.StatusInsufficientStorage)
	requireDetail(t, rec, "活动上传会话数已达上限")
	// The ledger and its repair still account for the completing session.
	if recomputed, _, err := app.Repo.RecomputeUploadQuota(); err != nil || recomputed != 4096 {
		t.Fatalf("recompute = %d (err %v), want 4096 while a session is completing", recomputed, err)
	}

	// A COMPLETE retried while the row is `completing` is refused without moving
	// anything -- the reservation and the slot stay where they are.
	requireStatus(t, completeUpload(t, app, uploadID, nil), http.StatusConflict)
	if reserved := reservedValue(t, app); reserved != 4096 {
		t.Fatalf("reserved = %d after the refused retry, want 4096", reserved)
	}
	// Roll the interrupted COMPLETE back (what FailComplete does in production):
	// the session is `active` again, still holding its bytes and its slot.
	if _, err := app.Repo.FailComplete(uploadID); err != nil {
		t.Fatalf("fail complete: %v", err)
	}
	if reserved := reservedValue(t, app); reserved != 4096 {
		t.Fatalf("reserved = %d after the rollback, want 4096", reserved)
	}
	// Completing releases the reservation and the slot in one transaction.
	requireStatus(t, completeUpload(t, app, uploadID, nil), http.StatusCreated)
	if reserved := reservedValue(t, app); reserved != 0 {
		t.Fatalf("reserved = %d after complete, want 0", reserved)
	}
	requireStatus(t, initRaw(t, app, "cap-3.bin", 4096, "env-cap", ""), http.StatusCreated)
}

// TestPurgeReclaimsStaleCompletingSessionThroughTheCleanupWorker drives the
// whole reclaim path through the handler helpers: a `completing` row that
// outlived the grace window is purged by the cleanup worker, its container is
// unlinked and its reservation comes back exactly once (no leak, no double
// release).
func TestPurgeReclaimsStaleCompletingSessionThroughTheCleanupWorker(t *testing.T) {
	app := newTestApp(t, smallChunkSettings())
	data := deterministicBytes(2048)
	init := initClipUpload(t, app, "stale.bin", int64(len(data)), "application/octet-stream", "env-stale-purge", nil)
	uploadID := init["uploadId"].(string)
	requireStatus(t, putChunk(t, app, uploadID, 0, data), http.StatusOK)

	session, err := app.Repo.GetUploadSession(uploadID)
	if err != nil || session == nil {
		t.Fatalf("load session: %v", err)
	}
	staged := session.StagedPath
	if _, statErr := os.Stat(staged); statErr != nil {
		t.Fatalf("the pre-allocated container must exist: %v", statErr)
	}
	if _, _, err := app.Repo.TryBeginComplete(uploadID); err != nil {
		t.Fatalf("begin complete: %v", err)
	}
	// Hard kill mid-COMPLETE: the row stays `completing`, expired and older than
	// the purge grace.
	now := time.Now().UTC().Unix()
	if err := app.Repo.SetUploadExpiryForTest(uploadID, now-600); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if _, err := app.Repo.AgeCompletingForTest(uploadID, repository.CompletingPurgeGraceSeconds+60); err != nil {
		t.Fatalf("age: %v", err)
	}

	app.purgeExpiredUploadsPeriodic()

	if cur, _ := app.Repo.GetUploadSession(uploadID); cur != nil {
		t.Fatalf("stale completing session must be purged, got %+v", cur)
	}
	if reserved, _ := app.Repo.UploadQuotaValue(); reserved != 0 {
		t.Fatalf("reserved = %d after the reclaim, want 0", reserved)
	}
	if recomputed, _, err := app.Repo.RecomputeUploadQuota(); err != nil || recomputed != 0 {
		t.Fatalf("recompute = %d (err %v), want 0", recomputed, err)
	}
	if _, statErr := os.Stat(staged); !os.IsNotExist(statErr) {
		t.Fatalf("the container of a purged session without a clip must be unlinked (err %v)", statErr)
	}
	// The reservation is not returned a second time by the next pass.
	app.purgeExpiredUploadsPeriodic()
	if reserved, _ := app.Repo.UploadQuotaValue(); reserved != 0 {
		t.Fatalf("reserved = %d after a second cleanup pass, want 0", reserved)
	}
}

// TestRollbackDanglingClipGuard covers the narrow interleaving the purge grace
// window cannot cover on its own: COMPLETE inserted its clip row, the cleanup
// worker reclaimed the (expired) session and unlinked the container, and only
// then does the completing->completed flip lose the row. Publishing that clip
// would be a 201 for a download that can only 404, so the guard deletes it --
// while a missing file is never guessed at (a stat error keeps the clip).
func TestRollbackDanglingClipGuard(t *testing.T) {
	app := newTestApp(t, smallChunkSettings())
	data := deterministicBytes(4096)
	init := initClipUpload(t, app, "dangling.bin", int64(len(data)), "application/octet-stream", "env-dangling", nil)
	uploadID := init["uploadId"].(string)
	requireStatus(t, putChunk(t, app, uploadID, 0, data), http.StatusOK)
	rec := completeUpload(t, app, uploadID, nil)
	requireStatus(t, rec, http.StatusCreated)
	clipID := decode(t, rec)["id"].(string)

	clip, err := app.Repo.GetClip(clipID)
	if err != nil || clip == nil || clip.StoredFile == nil {
		t.Fatalf("load clip: %+v (%v)", clip, err)
	}
	// The container is still there: the guard must leave the clip alone.
	if app.rollbackDanglingClip(clip) {
		t.Fatal("a clip whose file exists must never be rolled back")
	}
	if kept, _ := app.Repo.GetClip(clipID); kept == nil {
		t.Fatal("the clip must survive")
	}

	// Now the container is gone (a purge / manual delete): the guard removes the
	// clip row and returns its stored-bytes charge.
	if err := storage.RemoveUploadFile(clip.StoredFile.Path); err != nil {
		t.Fatalf("remove file: %v", err)
	}
	if !app.rollbackDanglingClip(clip) {
		t.Fatal("a clip whose file vanished must be rolled back")
	}
	if gone, _ := app.Repo.GetClip(clipID); gone != nil {
		t.Fatalf("the dangling clip must be deleted, got %+v", gone)
	}
	if used, err := app.Repo.ClipQuotaValue(); err != nil || used != 0 {
		t.Fatalf("stored-clip ledger = %d (err %v), want 0 after the rollback", used, err)
	}
	// A stat error means "unknown", and unknown keeps the clip.
	unknown := &models.Clip{
		ID: clipID, EnvironmentID: "env-dangling",
		StoredFile: &models.StoredFile{Name: "x", Size: 1, Mime: "text/plain", Path: ""},
	}
	if app.rollbackDanglingClip(unknown) {
		t.Fatal("a clip without a usable path must never be rolled back")
	}
	if app.rollbackDanglingClip(nil) {
		t.Fatal("nil clip must be ignored")
	}
}
