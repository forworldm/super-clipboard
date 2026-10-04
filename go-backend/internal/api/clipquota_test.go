package api

// End-to-end coverage of the stored-clip (saved file) quota:
//   - the bytes of a saved clip are charged when the clip row is inserted
//     (chunked-upload COMPLETE and the data-URL POST /api/clips path alike);
//   - an insert that does not fit is refused with 507, leaves no clip, no file
//     and no ledger movement, and the upload session stays resumable;
//   - every removal path (API delete, admin delete, TTL/download-limit cleanup)
//     returns exactly the bytes of the clip it removed, so the budget converges
//     back to the configured value.

import (
	"database/sql"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pixia1234/super-clipboard/backend/internal/config"
)

func storedQuotaSettings(quota int64) func(*config.Settings) {
	return func(s *config.Settings) {
		s.UploadChunkSizeBytes = 64 << 10 // min allowed (Effective clamps smaller)
		s.UploadSessionTTLSeconds = 3600
		s.CleanupIntervalSeconds = 3600
		s.StoredTotalQuotaBytes = quota
	}
}

func storedValue(t *testing.T, app *App) int64 {
	t.Helper()
	value, err := app.Repo.ClipQuotaValue()
	if err != nil {
		t.Fatalf("unable to read the clip storage ledger: %v", err)
	}
	if value < 0 {
		t.Fatalf("used bytes must never be negative, got %d", value)
	}
	return value
}

// uploadFile drives a complete chunked upload and returns the COMPLETE response
// plus the upload id (so a test can replay / complete again after a refusal).
func uploadFile(t *testing.T, app *App, filename string, data []byte, env string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	chunkSize := app.Settings.EffectiveChunkSize()
	init := initUpload(t, app, filename, int64(len(data)), "application/octet-stream", env)
	uploadID, ok := init["uploadId"].(string)
	if !ok {
		t.Fatalf("init response without uploadId: %v", init)
	}
	totalChunks := int(init["totalChunks"].(float64))
	for index := 0; index < totalChunks; index++ {
		requireStatus(t, putChunk(t, app, uploadID, index, sliceFor(chunkSize, data, index)), http.StatusOK)
	}
	return completeUpload(t, app, uploadID, nil), uploadID
}

// storedFileCount counts the assembled clip files in the storage root (the
// uploads/ chunk directory is deliberately excluded).
func storedFileCount(t *testing.T, app *App) int {
	t.Helper()
	entries, err := os.ReadDir(app.Settings.FileStorageDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("unable to list storage dir: %v", err)
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.Contains(entry.Name(), ".part.") {
			t.Fatalf("orphan assembly temp left behind: %s", entry.Name())
		}
		count++
	}
	return count
}

// expireClipNow rewrites the TTL of a stored clip through a second connection
// to the same database, simulating the passage of time (there is no public API
// to backdate a clip).
func expireClipNow(t *testing.T, app *App, clipID string) {
	t.Helper()
	handle, err := sql.Open("sqlite", app.Settings.DatabasePath)
	if err != nil {
		t.Fatalf("open side connection: %v", err)
	}
	defer handle.Close()
	if _, err := handle.Exec(
		"UPDATE clips SET expires_at = strftime('%s','now') - 60 WHERE id = ?", clipID); err != nil {
		t.Fatalf("unable to expire clip %s: %v", clipID, err)
	}
}

// ---------------------------------------------------------------------------
// charging / releasing through the public API
// ---------------------------------------------------------------------------

// TestStoredQuotaChargedAndReleasedThroughTheAPI walks the happy path: the
// saved bytes are visible in the ledger, the clip is listed, deleting it gives
// the bytes back, and text clips never consume the budget.
func TestStoredQuotaChargedAndReleasedThroughTheAPI(t *testing.T) {
	app := newTestApp(t, storedQuotaSettings(1000))
	env := "env-stored"

	rec, _ := uploadFile(t, app, "charged.bin", deterministicBytes(400), env)
	requireStatus(t, rec, http.StatusCreated)
	clipID := decode(t, rec)["id"].(string)
	if got := storedValue(t, app); got != 400 {
		t.Fatalf("used = %d, want 400 after saving a 400 byte clip", got)
	}

	listed := do(t, app, http.MethodGet, "/api/clips?environmentId="+env, nil)
	requireStatus(t, listed, http.StatusOK)
	if items := decode(t, listed)["items"].([]interface{}); len(items) != 1 {
		t.Fatalf("expected 1 clip, got %d", len(items))
	}

	// Text clips carry no file and therefore no charge.
	createTextClipForEnv(t, app, env, "free as in beer")
	if got := storedValue(t, app); got != 400 {
		t.Fatalf("used = %d, want 400 (text clips are free)", got)
	}

	deleted := do(t, app, http.MethodDelete, "/api/clips/"+clipID+"?environmentId="+env, nil)
	requireStatus(t, deleted, http.StatusOK)
	if got := storedValue(t, app); got != 0 {
		t.Fatalf("used = %d, want 0 after deleting the clip", got)
	}
	if count := storedFileCount(t, app); count != 0 {
		t.Fatalf("expected no stored files left, got %d", count)
	}
}

// TestStoredQuotaRefusalAtComplete507 pins the refusal contract of the chunked
// upload path: HTTP 507, a typed detail, no clip, no ledger movement, the
// pre-allocated container kept (the bytes already sit in their final position,
// so a rollback has nothing to copy or discard) and the session still resumable.
// Deleting the clip that owns the bytes makes the very same upload succeed -- the
// budget converges; only THEN does the second container stop being extra space.
func TestStoredQuotaRefusalAtComplete507(t *testing.T) {
	app := newTestApp(t, storedQuotaSettings(200))
	env := "env-refuse"

	first, _ := uploadFile(t, app, "first.bin", deterministicBytes(150), env)
	requireStatus(t, first, http.StatusCreated)
	firstID := decode(t, first)["id"].(string)
	if got := storedValue(t, app); got != 150 {
		t.Fatalf("used = %d, want 150", got)
	}

	rec, uploadID := uploadFile(t, app, "second.bin", deterministicBytes(150), env)
	requireStatus(t, rec, http.StatusInsufficientStorage)
	detail, _ := decode(t, rec)["detail"].(string)
	if !strings.Contains(detail, "存储配额不足") {
		t.Fatalf("unexpected 507 detail %q", detail)
	}
	if got := storedValue(t, app); got != 150 {
		t.Fatalf("a refused clip must not move the ledger: %d", got)
	}
	// Two containers exist: the completed clip's file and the rolled-back
	// session's pre-allocated file (kept so the retry can finish without
	// re-uploading, exactly like the old chunk files were kept).
	if count := storedFileCount(t, app); count != 2 {
		t.Fatalf("the refused upload must keep exactly its own container: %d files", count)
	}
	listed := do(t, app, http.MethodGet, "/api/clips?environmentId="+env, nil)
	requireStatus(t, listed, http.StatusOK)
	if items := decode(t, listed)["items"].([]interface{}); len(items) != 1 {
		t.Fatalf("a refused upload must not create a clip: %d clips", len(items))
	}

	// The session rolled back to `active` with all its chunks: resumable.
	status := getUpload(t, app, uploadID)
	requireStatus(t, status, http.StatusOK)
	body := decode(t, status)
	if body["status"] != "active" {
		t.Fatalf("session status = %v, want active after the refusal", body["status"])
	}
	received := body["receivedChunks"].([]interface{})
	if len(received) != int(body["totalChunks"].(float64)) {
		t.Fatalf("chunks must survive the refusal: %v", body)
	}

	// Free the bytes and retry the identical complete call.
	deleted := do(t, app, http.MethodDelete, "/api/clips/"+firstID+"?environmentId="+env, nil)
	requireStatus(t, deleted, http.StatusOK)
	if got := storedValue(t, app); got != 0 {
		t.Fatalf("used = %d, want 0", got)
	}
	retry := completeUpload(t, app, uploadID, nil)
	requireStatus(t, retry, http.StatusCreated)
	if got := storedValue(t, app); got != 150 {
		t.Fatalf("used = %d, want 150 after the retry", got)
	}
	// The retried complete needed no copy: one container per surviving clip.
	if count := storedFileCount(t, app); count != 1 {
		t.Fatalf("expected exactly the retried clip's file, got %d", count)
	}
}

// TestStoredQuotaRefusalOnDataURLClip507 covers the other entry point: a file
// clip submitted inline as a data URL is refused for the same reason, and the
// bytes it already wrote are cleaned up.
func TestStoredQuotaRefusalOnDataURLClip507(t *testing.T) {
	app := newTestApp(t, storedQuotaSettings(5))
	env := "env-dataurl"

	payload := deterministicBytes(64)
	rec := do(t, app, http.MethodPost, "/api/clips", map[string]interface{}{
		"type":          "file",
		"expiresAt":     futureTimestamp(2),
		"environmentId": env,
		"payload": map[string]interface{}{
			"file": map[string]interface{}{
				"name":    "inline.bin",
				"size":    len(payload),
				"type":    "application/octet-stream",
				"dataUrl": "data:application/octet-stream;base64," + base64.StdEncoding.EncodeToString(payload),
			},
		},
	})
	requireStatus(t, rec, http.StatusInsufficientStorage)
	if got := storedValue(t, app); got != 0 {
		t.Fatalf("used = %d, want 0 after a refused clip", got)
	}
	if count := storedFileCount(t, app); count != 0 {
		t.Fatalf("a refused inline file must be removed, %d files left", count)
	}

	// Text clips are unaffected by a full file budget.
	createTextClipForEnv(t, app, env, "still works")
	if got := storedValue(t, app); got != 0 {
		t.Fatalf("used = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// releasing: cleanup paths must keep the ledger in sync (requirement 2.2)
// ---------------------------------------------------------------------------

// TestStoredQuotaReleasedByEveryRemovalPath covers the paths that delete clip
// rows besides an explicit user delete: the periodic PurgeInactive sweep that
// GET /api/clips (and the cleanup worker / startup) triggers for expired clips,
// and the admin delete endpoint.
func TestStoredQuotaReleasedByEveryRemovalPath(t *testing.T) {
	app := newTestApp(t, func(s *config.Settings) {
		s.UploadChunkSizeBytes = 64 << 10
		s.UploadSessionTTLSeconds = 3600
		s.CleanupIntervalSeconds = 3600
		s.StoredTotalQuotaBytes = 10_000
		s.AdminAPIKey = "admin-secret"
	})
	env := "env-cleanup"

	expiredRec, _ := uploadFile(t, app, "expired.bin", deterministicBytes(700), env)
	requireStatus(t, expiredRec, http.StatusCreated)
	expiredID := decode(t, expiredRec)["id"].(string)

	keptRec, _ := uploadFile(t, app, "kept.bin", deterministicBytes(300), env)
	requireStatus(t, keptRec, http.StatusCreated)
	keptID := decode(t, keptRec)["id"].(string)

	if got := storedValue(t, app); got != 1000 {
		t.Fatalf("used = %d, want 1000", got)
	}

	// TTL expiry + the purge GET /api/clips performs before listing.
	expireClipNow(t, app, expiredID)
	listed := do(t, app, http.MethodGet, "/api/clips?environmentId="+env, nil)
	requireStatus(t, listed, http.StatusOK)
	if items := decode(t, listed)["items"].([]interface{}); len(items) != 1 {
		t.Fatalf("expected the expired clip to be purged, got %d clips", len(items))
	}
	if got := storedValue(t, app); got != 300 {
		t.Fatalf("used = %d, want 300 after the expiry cleanup", got)
	}
	if count := storedFileCount(t, app); count != 1 {
		t.Fatalf("the purged clip file must be removed, %d files left", count)
	}

	// Admin delete (no environmentId needed: the endpoint resolves the owner).
	adminRec := doAdmin(t, app, http.MethodDelete, "/api/admin/clips/"+keptID, "admin-secret")
	requireStatus(t, adminRec, http.StatusOK)
	if got := storedValue(t, app); got != 0 {
		t.Fatalf("used = %d, want 0 after the admin delete", got)
	}
	if count := storedFileCount(t, app); count != 0 {
		t.Fatalf("expected an empty storage dir, got %d files", count)
	}
}

// TestStoredQuotaReleasedByDownloadLimitSweep: a clip whose download budget is
// exhausted is deleted by the API (DELETE after the last download) and by the
// periodic purge -- both must return its bytes.
func TestStoredQuotaReleasedByDownloadLimitSweep(t *testing.T) {
	app := newTestApp(t, storedQuotaSettings(1000))
	env := "env-downloads"

	rec, _ := uploadFile(t, app, "once.bin", deterministicBytes(250), env)
	requireStatus(t, rec, http.StatusCreated)
	clipID := decode(t, rec)["id"].(string)
	// Cap it at one download (the upload's default cap is 10).
	handle, err := sql.Open("sqlite", app.Settings.DatabasePath)
	if err != nil {
		t.Fatalf("open side connection: %v", err)
	}
	if _, err := handle.Exec("UPDATE clips SET max_downloads = 1 WHERE id = ?", clipID); err != nil {
		handle.Close()
		t.Fatalf("unable to cap downloads: %v", err)
	}
	handle.Close()

	download := do(t, app, http.MethodGet, "/api/clips/"+clipID+"/file?environmentId="+env, nil)
	requireStatus(t, download, http.StatusOK)
	// The last download schedules an asynchronous delete.
	waitFor(t, func() bool { value, _ := app.Repo.ClipQuotaValue(); return value == 0 })
	if got := storedValue(t, app); got != 0 {
		t.Fatalf("used = %d, want 0 after the exhausted download", got)
	}
}

// waitFor polls a condition for a short while (the download path deletes the
// clip in a goroutine, mirroring FastAPI's background task).
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	for attempt := 0; attempt < 200; attempt++ {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

// TestStoredQuotaReconciledOnStartupAndByWorker: a drifting ledger is repaired
// by the same code the cleanup worker and the startup recovery run.
func TestStoredQuotaReconciledOnStartupAndByWorker(t *testing.T) {
	app := newTestApp(t, storedQuotaSettings(100_000))
	env := "env-reconcile"

	rec, _ := uploadFile(t, app, "drift.bin", deterministicBytes(500), env)
	requireStatus(t, rec, http.StatusCreated)

	if err := app.Repo.ForceClipQuotaForTest(999_999); err != nil {
		t.Fatalf("force drift: %v", err)
	}
	// The worker tick.
	app.reconcileClipQuota()
	if got := storedValue(t, app); got != 500 {
		t.Fatalf("used = %d, want 500 after the worker repair", got)
	}

	if err := app.Repo.ForceClipQuotaForTest(0); err != nil {
		t.Fatalf("force drift: %v", err)
	}
	// The startup path (step 7 of ReconcileUploadsOnStartup).
	app.ReconcileUploadsOnStartup()
	if got := storedValue(t, app); got != 500 {
		t.Fatalf("used = %d, want 500 after the startup repair", got)
	}
	if authoritative, err := app.Repo.StoredClipBytes(); err != nil || authoritative != 500 {
		t.Fatalf("SUM(file_size) = %d (%v), want 500", authoritative, err)
	}
}

// TestStoredQuotaConcurrentCompletesNoOversell drives the whole HTTP stack in
// parallel: a 250 byte budget fits exactly two 100 byte files, so exactly two
// COMPLETE calls succeed, two answer 507, and the ledger accounts for the two
// clips that were really stored (no oversell, no lost charge).
func TestStoredQuotaConcurrentCompletesNoOversell(t *testing.T) {
	app := newTestApp(t, storedQuotaSettings(250))
	env := "env-race"

	var (
		created int64
		refused int64
		other   int64
		wg      sync.WaitGroup
	)
	start := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			rec, _ := uploadFile(t, app, fmt.Sprintf("race-%d.bin", index), deterministicBytes(100), env)
			switch rec.Code {
			case http.StatusCreated:
				atomic.AddInt64(&created, 1)
			case http.StatusInsufficientStorage:
				atomic.AddInt64(&refused, 1)
			default:
				atomic.AddInt64(&other, 1)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if other != 0 {
		t.Fatalf("unexpected COMPLETE responses: %d", other)
	}
	if created != 2 || refused != 2 {
		t.Fatalf("created = %d, refused = %d; want 2 and 2", created, refused)
	}
	if got := storedValue(t, app); got != 200 {
		t.Fatalf("used = %d, want 200 (two 100 byte clips, a third would exceed 250)", got)
	}
	if authoritative, err := app.Repo.StoredClipBytes(); err != nil || authoritative != 200 {
		t.Fatalf("SUM(file_size) = %d (%v), want 200", authoritative, err)
	}
	listed := do(t, app, http.MethodGet, "/api/clips?environmentId="+env, nil)
	requireStatus(t, listed, http.StatusOK)
	if items := decode(t, listed)["items"].([]interface{}); len(items) != 2 {
		t.Fatalf("expected 2 stored clips, got %d", len(items))
	}
	// 4 containers: the 2 clips that won own 2 files, and the 2 refused sessions
	// keep their own pre-allocated file for a possible retry (no copy was made
	// for the winners, which is what used to double the disk usage).
	if count := storedFileCount(t, app); count != 4 {
		t.Fatalf("expected 2 clip files + 2 kept containers, got %d", count)
	}
}
