package api

import (
	"bytes"
	"encoding/json"
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
	"github.com/pixia1234/super-clipboard/backend/internal/storage"
)

// The whole suite runs against a deterministic free-space probe so a laptop
// with a full disk still sees green tests. Individual tests override
// App.freeBytesFn to exercise the watermark.
func init() {
	defaultFreeDiskBytes = func(string) (int64, error) { return 1 << 40, nil }
}

func quotaSettings(quota int64, maxSessions int, minFree int64) func(*config.Settings) {
	return func(s *config.Settings) {
		s.UploadChunkSizeBytes = 64 << 10
		s.UploadSessionTTLSeconds = 3600
		s.CleanupIntervalSeconds = 3600
		s.UploadTotalQuotaBytes = quota
		s.MaxActiveUploadSessions = maxSessions
		s.MinFreeDiskBytes = minFree
	}
}

// initRaw performs an upload init and returns the raw response so tests can
// assert on the refusal paths as well.
func initRaw(t *testing.T, app *App, filename string, fileSize int64, env, requestID string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]interface{}{
		"filename": filename, "fileSize": fileSize,
		"mimeType": "application/octet-stream", "environmentId": env,
		// Every upload is a clip: expiresAt is part of the init contract.
		"expiresAt": futureTimestamp(2),
	}
	if requestID != "" {
		body["requestId"] = requestID
	}
	return do(t, app, http.MethodPost, "/api/uploads/init", body)
}

func reservedValue(t *testing.T, app *App) int64 {
	t.Helper()
	value, err := app.Repo.UploadQuotaValue()
	if err != nil {
		t.Fatalf("unable to read upload quota: %v", err)
	}
	if value < 0 {
		t.Fatalf("reserved bytes must never be negative, got %d", value)
	}
	return value
}

func activeSessions(t *testing.T, app *App) int {
	t.Helper()
	sessions, err := app.Repo.ListUploadSessions()
	if err != nil {
		t.Fatalf("unable to list sessions: %v", err)
	}
	return len(sessions)
}

func requireDetail(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	payload := decode(t, rec)
	detail, _ := payload["detail"].(string)
	if !strings.Contains(detail, want) {
		t.Fatalf("detail %q does not contain %q", detail, want)
	}
}

// TestInitQuotaBoundaryAndRefusal: a reservation landing exactly on the budget
// is accepted, one more byte is refused with 507 and leaves no trace behind.
func TestInitQuotaBoundaryAndRefusal(t *testing.T) {
	app := newTestApp(t, quotaSettings(1000, 0, 0))

	firstRec := initRaw(t, app, "a.bin", 600, "env-q", "")
	requireStatus(t, firstRec, http.StatusCreated)
	bigID, _ := decode(t, firstRec)["uploadId"].(string)
	if got := reservedValue(t, app); got != 600 {
		t.Fatalf("reserved = %d, want 600", got)
	}
	requireStatus(t, initRaw(t, app, "b.bin", 400, "env-q", ""), http.StatusCreated)
	if got := reservedValue(t, app); got != 1000 {
		t.Fatalf("reserved = %d, want 1000 (exact budget hit)", got)
	}

	sessionsBefore := activeSessions(t, app)
	rec := initRaw(t, app, "c.bin", 1, "env-q", "")
	requireStatus(t, rec, http.StatusInsufficientStorage)
	requireDetail(t, rec, "上传配额不足")
	if got := reservedValue(t, app); got != 1000 {
		t.Fatalf("a refused init moved the ledger to %d", got)
	}
	if got := activeSessions(t, app); got != sessionsBefore {
		t.Fatalf("a refused init created a session (%d -> %d)", sessionsBefore, got)
	}
	// Exactly the two accepted inits own a storage file; the refused one owns none.
	if got := storageFileCount(t, app); got != 2 {
		t.Fatalf("a refused init must not allocate a file, found %d (expected 2)", got)
	}

	// Freeing the 600 byte session re-opens exactly that much budget.
	requireStatus(t, do(t, app, http.MethodDelete, "/api/uploads/"+bigID, nil), http.StatusOK)
	if got := reservedValue(t, app); got != 400 {
		t.Fatalf("reserved = %d after abort, want 400", got)
	}
	requireStatus(t, initRaw(t, app, "c.bin", 600, "env-q", ""), http.StatusCreated)
	if got := reservedValue(t, app); got != 1000 {
		t.Fatalf("reserved = %d after re-booking, want 1000", got)
	}
}

// firstUploadID returns the id of the oldest session (creation order).
func firstUploadID(t *testing.T, app *App) string {
	t.Helper()
	sessions, err := app.Repo.ListUploadSessions()
	if err != nil || len(sessions) == 0 {
		t.Fatalf("no sessions: %v", err)
	}
	return sessions[0].ID
}

// TestInitQuotaConcurrentNoOversell: N parallel inits against a budget that
// fits M of them must create exactly M sessions and answer 507 to the rest.
func TestInitQuotaConcurrentNoOversell(t *testing.T) {
	app := newTestApp(t, quotaSettings(1000, 0, 0))

	const (
		attempts   = 32
		perRequest = 100
		wantWins   = 10
	)
	var created, refused int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			rec := initRaw(t, app, fmt.Sprintf("f%d.bin", i), perRequest, "env-race", fmt.Sprintf("req-%d", i))
			switch rec.Code {
			case http.StatusCreated:
				atomic.AddInt64(&created, 1)
			case http.StatusInsufficientStorage:
				atomic.AddInt64(&refused, 1)
			default:
				t.Errorf("unexpected status %d: %s", rec.Code, rec.Body.String())
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if atomic.LoadInt64(&created) != wantWins || atomic.LoadInt64(&refused) != attempts-wantWins {
		t.Fatalf("created %d / refused %d, want %d / %d", created, refused, wantWins, attempts-wantWins)
	}
	if got := reservedValue(t, app); got != wantWins*perRequest {
		t.Fatalf("reserved = %d, want %d", got, wantWins*perRequest)
	}
	if got := activeSessions(t, app); got != wantWins {
		t.Fatalf("sessions = %d, want %d", got, wantWins)
	}
}

// TestInitSessionLimit507 caps the number of concurrent upload sessions.
func TestInitSessionLimit507(t *testing.T) {
	app := newTestApp(t, quotaSettings(0, 2, 0))

	requireStatus(t, initRaw(t, app, "s0.bin", 100, "env-l", ""), http.StatusCreated)
	requireStatus(t, initRaw(t, app, "s1.bin", 100, "env-l", ""), http.StatusCreated)

	rec := initRaw(t, app, "s2.bin", 100, "env-l", "")
	requireStatus(t, rec, http.StatusInsufficientStorage)
	requireDetail(t, rec, "活动上传会话数已达上限")
	if got := activeSessions(t, app); got != 2 {
		t.Fatalf("sessions = %d, want 2", got)
	}
	// The refusal must not have consumed quota either.
	if got := reservedValue(t, app); got != 200 {
		t.Fatalf("reserved = %d, want 200", got)
	}

	// Cancelling one session frees the slot.
	requireStatus(t, do(t, app, http.MethodDelete, "/api/uploads/"+firstUploadID(t, app), nil), http.StatusOK)
	requireStatus(t, initRaw(t, app, "s2.bin", 100, "env-l", ""), http.StatusCreated)
}

// TestInitDiskWatermark507 guards the free-space watermark: no session, no
// bytes, no reservation may survive a refusal.
func TestInitDiskWatermark507(t *testing.T) {
	app := newTestApp(t, quotaSettings(0, 0, 1<<20))
	app.freeBytesFn = func(string) (int64, error) { return 1<<20 - 1, nil }

	rec := initRaw(t, app, "d.bin", 4096, "env-d", "")
	requireStatus(t, rec, http.StatusInsufficientStorage)
	requireDetail(t, rec, "磁盘可用空间不足")
	if got := reservedValue(t, app); got != 0 {
		t.Fatalf("a refused init left %d bytes reserved", got)
	}
	if got := activeSessions(t, app); got != 0 {
		t.Fatalf("a refused init created %d session(s)", got)
	}
	if got := storageFileCount(t, app); got != 0 {
		t.Fatalf("a refused init must not allocate a file, found %d", got)
	}

	// Exactly at the watermark is fine, and an unreadable probe fails open.
	app.freeBytesFn = func(string) (int64, error) { return 1 << 20, nil }
	requireStatus(t, initRaw(t, app, "d.bin", 4096, "env-d", ""), http.StatusCreated)
	app.freeBytesFn = func(string) (int64, error) { return 0, errProbe }
	requireStatus(t, initRaw(t, app, "e.bin", 4096, "env-d", ""), http.StatusCreated)
}

var errProbe = fmt.Errorf("statfs unavailable")

// TestChunkWriteBlockedByDiskWatermark: a chunk is never streamed to a volume
// below the watermark.
func TestChunkWriteBlockedByDiskWatermark(t *testing.T) {
	app := newTestApp(t, quotaSettings(0, 0, 1<<20))
	app.freeBytesFn = func(string) (int64, error) { return 1 << 30, nil }

	payload := initUpload(t, app, "chunk.bin", 2048, "application/octet-stream", "env-c")
	uploadID, _ := payload["uploadId"].(string)
	data := deterministicBytes(2048)

	app.freeBytesFn = func(string) (int64, error) { return 1024, nil }
	rec := putChunk(t, app, uploadID, 0, data)
	requireStatus(t, rec, http.StatusInsufficientStorage)
	requireDetail(t, rec, "磁盘可用空间不足")
	// The pre-allocated file exists (init created it) but must still be all zeros:
	// not a single payload byte may be written below the watermark.
	session, _ := app.Repo.GetUploadSession(uploadID)
	if session == nil || session.StagedPath == "" {
		t.Fatalf("session should own its pre-allocated file: %+v", session)
	}
	stored, err := os.ReadFile(session.StagedPath)
	if err != nil {
		t.Fatalf("read pre-allocated file: %v", err)
	}
	if !bytes.Equal(stored, make([]byte, len(data))) {
		t.Fatalf("payload landed on disk below the watermark (%d bytes)", len(stored))
	}
	received, err := app.Repo.ListReceivedChunks(uploadID)
	if err != nil {
		t.Fatalf("list chunks: %v", err)
	}
	if len(received) != 0 {
		t.Fatalf("chunk receipt recorded below the watermark: %v", received)
	}

	// Watermark recovered: the very same chunk is accepted.
	app.freeBytesFn = func(string) (int64, error) { return 1 << 30, nil }
	requireStatus(t, putChunk(t, app, uploadID, 0, data), http.StatusOK)
}

// storageFileCount counts the upload containers a test app currently owns
// (a refused init must leave the storage dir untouched).
func storageFileCount(t *testing.T, app *App) int {
	t.Helper()
	names, err := storage.ListStorageFiles(app.Settings.FileStorageDir)
	if err != nil {
		t.Fatalf("list storage files: %v", err)
	}
	return len(names)
}

// TestInitReplayDoesNotReserveTwice keeps idempotent init from eating budget.
func TestInitReplayDoesNotReserveTwice(t *testing.T) {
	app := newTestApp(t, quotaSettings(1000, 0, 0))

	first := initRaw(t, app, "r.bin", 500, "env-idem", "req-1")
	requireStatus(t, first, http.StatusCreated)
	var firstPayload map[string]interface{}
	if err := json.Unmarshal(first.Body.Bytes(), &firstPayload); err != nil {
		t.Fatalf("decode: %v", err)
	}

	second := initRaw(t, app, "r.bin", 500, "env-idem", "req-1")
	requireStatus(t, second, http.StatusOK)
	var secondPayload map[string]interface{}
	if err := json.Unmarshal(second.Body.Bytes(), &secondPayload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if firstPayload["uploadId"] != secondPayload["uploadId"] {
		t.Fatalf("replay returned a different session: %v vs %v", firstPayload["uploadId"], secondPayload["uploadId"])
	}
	if got := reservedValue(t, app); got != 500 {
		t.Fatalf("replay reserved twice: %d", got)
	}
}

// TestAbortReleasesQuota / complete / expiry all give the bytes back.
func TestQuotaReleasedOnAbortCompleteAndExpiry(t *testing.T) {
	t.Run("abort", func(t *testing.T) {
		app := newTestApp(t, quotaSettings(1000, 0, 0))
		payload := initUpload(t, app, "abort.bin", 500, "application/octet-stream", "env-a")
		uploadID, _ := payload["uploadId"].(string)
		requireStatus(t, do(t, app, http.MethodDelete, "/api/uploads/"+uploadID, nil), http.StatusOK)
		if got := reservedValue(t, app); got != 0 {
			t.Fatalf("reserved = %d after abort, want 0", got)
		}
		// Aborting twice (client retry) must not double-return.
		requireStatus(t, do(t, app, http.MethodDelete, "/api/uploads/"+uploadID, nil), http.StatusNotFound)
		if got := reservedValue(t, app); got != 0 {
			t.Fatalf("reserved = %d after second abort, want 0", got)
		}
	})

	t.Run("complete", func(t *testing.T) {
		app := newTestApp(t, quotaSettings(1000, 0, 0))
		payload := initUpload(t, app, "done.bin", 800, "application/octet-stream", "env-c")
		uploadID, _ := payload["uploadId"].(string)
		requireStatus(t, putChunk(t, app, uploadID, 0, deterministicBytes(800)), http.StatusOK)
		done := completeUpload(t, app, uploadID, nil)
		requireStatus(t, done, http.StatusCreated)
		if decode(t, done)["type"] != "file" {
			t.Fatalf("complete must insert a file clip, got %s", done.Body.String())
		}
		if got := reservedValue(t, app); got != 0 {
			t.Fatalf("reserved = %d after complete, want 0", got)
		}
		// Idempotent replay of complete keeps the ledger untouched.
		requireStatus(t, completeUpload(t, app, uploadID, nil), http.StatusOK)
		if got := reservedValue(t, app); got != 0 {
			t.Fatalf("reserved = %d after complete replay, want 0", got)
		}
	})

	t.Run("expiry", func(t *testing.T) {
		app := newTestApp(t, quotaSettings(1000, 0, 0))
		payload := initUpload(t, app, "exp.bin", 700, "application/octet-stream", "env-e")
		uploadID, _ := payload["uploadId"].(string)
		if err := app.Repo.SetUploadExpiryForTest(uploadID, 1); err != nil {
			t.Fatalf("force expiry: %v", err)
		}
		requireStatus(t, getUpload(t, app, uploadID), http.StatusGone)
		if got := reservedValue(t, app); got != 0 {
			t.Fatalf("reserved = %d after expiry cleanup, want 0", got)
		}
	})
}

// TestConcurrentApiReleaseIdempotent races abort, expiry cleanup and the
// periodic purge over one session: the bytes must come back exactly once.
func TestConcurrentApiReleaseIdempotent(t *testing.T) {
	app := newTestApp(t, quotaSettings(0, 0, 0))
	initUpload(t, app, "sentinel.bin", 3000, "application/octet-stream", "env-s")
	payload := initUpload(t, app, "race.bin", 700, "application/octet-stream", "env-s")
	uploadID, _ := payload["uploadId"].(string)
	if err := app.Repo.SetUploadExpiryForTest(uploadID, 1); err != nil {
		t.Fatalf("force expiry: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			do(t, app, http.MethodDelete, "/api/uploads/"+uploadID, nil)
		}()
		go func() {
			defer wg.Done()
			getUpload(t, app, uploadID) // expired -> synchronous purge
		}()
		go func() {
			defer wg.Done()
			if _, err := app.Repo.PurgeExpiredUploads(time.Now().UTC().Unix()); err != nil {
				t.Errorf("purge: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := reservedValue(t, app); got != 3000 {
		t.Fatalf("reserved = %d, want the 3000 byte sentinel (700 returned exactly once)", got)
	}
}

// TestCleanupWorkerReconcilesQuotaDrift repairs a drifting ledger.
func TestCleanupWorkerReconcilesQuotaDrift(t *testing.T) {
	app := newTestApp(t, quotaSettings(1000, 0, 0))
	initUpload(t, app, "drift.bin", 500, "application/octet-stream", "env-drift")

	if err := app.Repo.ForceUploadQuotaForTest(99999); err != nil {
		t.Fatalf("inject drift: %v", err)
	}
	app.reconcileUploadQuota()
	if got := reservedValue(t, app); got != 500 {
		t.Fatalf("reserved = %d after reconciliation, want 500", got)
	}
	// Running the worker again is a no-op.
	app.reconcileUploadQuota()
	if got := reservedValue(t, app); got != 500 {
		t.Fatalf("reserved = %d after second reconciliation, want 500", got)
	}
}

// TestStorageGuardsAnswerDistinct507 keeps the three refusals distinguishable.
func TestStorageGuardsAnswerDistinct507(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(*App)
		want   string
		status int
	}{
		{
			name:   "quota",
			setup:  func(a *App) { a.Settings.UploadTotalQuotaBytes = 10 },
			want:   "上传配额不足",
			status: http.StatusInsufficientStorage,
		},
		{
			name: "sessions",
			setup: func(a *App) {
				a.Settings.MaxActiveUploadSessions = 1
				a.Settings.UploadTotalQuotaBytes = 0
				// The slot is taken by a first session before the assertion.
				initRaw(t, a, "seed.bin", 64, "env-g", "")
			},
			want:   "活动上传会话数已达上限",
			status: http.StatusInsufficientStorage,
		},
		{
			name: "disk",
			setup: func(a *App) {
				a.Settings.UploadTotalQuotaBytes = 0
				a.Settings.MinFreeDiskBytes = 1 << 30
				a.freeBytesFn = func(string) (int64, error) { return 1, nil }
			},
			want:   "磁盘可用空间不足",
			status: http.StatusInsufficientStorage,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			app := newTestApp(t, quotaSettings(0, 0, 0))
			c.setup(app)
			rec := initRaw(t, app, "g.bin", 64, "env-g", "")
			requireStatus(t, rec, c.status)
			requireDetail(t, rec, c.want)
		})
	}
}

// errPreallocate stands in for a failing container creation (ENOSPC, EROFS, ...).
var errPreallocate = fmt.Errorf("pre-allocation failed")

// TestInitPreallocationFailureReturnsReservation covers the init error path that
// used to be hardest to reason about: the session row is committed (so it owns
// the reservation) and the container creation then fails. The abort returns the
// reservation through the quota_released CAS exactly ONCE -- the handler's
// deferred compensation must not decrement a second time -- so a live sentinel
// reservation is left untouched and the idempotency key is free for the retry.
func TestInitPreallocationFailureReturnsReservation(t *testing.T) {
	app := newTestApp(t, quotaSettings(2000, 0, 0))
	const sentinelBytes int64 = 700
	sentinel := initUpload(t, app, "sentinel.bin", sentinelBytes, "application/octet-stream", "env-pre-fail")
	sentinelID := sentinel["uploadId"].(string)
	if got := storageFileCount(t, app); got != 1 {
		t.Fatalf("sentinel container missing: %v", storageFileNames(t, app))
	}

	app.preallocateUploadFn = func(string, int64) error { return errPreallocate }
	rec := initRaw(t, app, "fail.bin", 500, "env-pre-fail", "req-fail-1")
	requireStatus(t, rec, http.StatusInternalServerError)
	requireDetail(t, rec, "无法创建上传文件")

	// Row gone, container gone, and exactly the sentinel's 700 bytes reserved.
	if got := reservedValue(t, app); got != sentinelBytes {
		t.Fatalf("reserved = %d after a failed init, want the %d byte sentinel", got, sentinelBytes)
	}
	if got := activeSessions(t, app); got != 1 {
		t.Fatalf("a failed init must leave exactly the sentinel session, got %d", got)
	}
	if names := storageFileNames(t, app); len(names) != 1 {
		t.Fatalf("a failed init must leave no container behind, got %v", names)
	}

	// The key was released with the row: retrying the very same init works.
	app.preallocateUploadFn = nil
	retry := initRaw(t, app, "fail.bin", 500, "env-pre-fail", "req-fail-1")
	requireStatus(t, retry, http.StatusCreated)
	retryID := decode(t, retry)["uploadId"].(string)
	if retryID == sentinelID {
		t.Fatal("the retry must create a fresh session, not replay the failed one")
	}
	if got := reservedValue(t, app); got != 1200 {
		t.Fatalf("reserved = %d after the retry, want 1200 (700 sentinel + 500 upload)", got)
	}

	// Both sessions can still be returned, one at a time.
	requireStatus(t, do(t, app, http.MethodDelete, "/api/uploads/"+sentinelID, nil), http.StatusOK)
	if got := reservedValue(t, app); got != 500 {
		t.Fatalf("reserved = %d after aborting the sentinel, want 500", got)
	}
	requireStatus(t, do(t, app, http.MethodDelete, "/api/uploads/"+retryID, nil), http.StatusOK)
	if got := reservedValue(t, app); got != 0 {
		t.Fatalf("reserved = %d after aborting both sessions, want 0", got)
	}
}

// TestPurgeAfterCompleteDoesNotReleaseQuotaTwice drives the "release twice" race
// through the API: the successful COMPLETE returns the reservation, and the
// cleanup worker then purges the finished session row (which carries the
// quota_released CAS flag). The 700 byte sentinel proves the ledger never moved
// a second time, and the clip keeps its bytes and serves its file.
func TestPurgeAfterCompleteDoesNotReleaseQuotaTwice(t *testing.T) {
	app := newTestApp(t, quotaSettings(2000, 0, 0))
	env := "env-purge-once"
	const sentinelBytes int64 = 1200
	sentinel := initUpload(t, app, "sentinel.bin", sentinelBytes, "application/octet-stream", env)
	sentinelID := sentinel["uploadId"].(string)

	payload := initClipUpload(t, app, "finished.bin", 500, "application/octet-stream", env,
		map[string]interface{}{"requestId": "req-purge-once"})
	uploadID := payload["uploadId"].(string)
	requireStatus(t, putChunk(t, app, uploadID, 0, deterministicBytes(500)), http.StatusOK)
	done := completeUpload(t, app, uploadID, nil)
	requireStatus(t, done, http.StatusCreated)
	clipID := decode(t, done)["id"].(string)
	if got := reservedValue(t, app); got != sentinelBytes {
		t.Fatalf("complete must return the 500 bytes exactly once, reserved = %d", got)
	}
	filesBefore := storageFileCount(t, app)

	// Replay window elapsed: the worker purges the finished row.
	if err := app.Repo.SetUploadExpiryForTest(uploadID, 1); err != nil {
		t.Fatalf("force expiry: %v", err)
	}
	app.purgeExpiredUploadsPeriodic()

	if purged, _ := app.Repo.GetUploadSession(uploadID); purged != nil {
		t.Fatalf("the finished session row should be purged, got %+v", purged)
	}
	if got := reservedValue(t, app); got != sentinelBytes {
		t.Fatalf("purge released the finished reservation a second time: reserved = %d, want %d",
			got, sentinelBytes)
	}
	// The clip and its file are independent of the session row: both survive.
	if got := storageFileCount(t, app); got != filesBefore {
		t.Fatalf("purging a finished session must keep the clip file: %d -> %d (%v)",
			filesBefore, got, storageFileNames(t, app))
	}
	requireStatus(t, do(t, app, http.MethodGet, "/api/clips/"+clipID+"/file?environmentId="+env, nil), http.StatusOK)

	// Idempotent maintenance: a second pass changes nothing.
	app.purgeExpiredUploadsPeriodic()
	if got := reservedValue(t, app); got != sentinelBytes {
		t.Fatalf("reserved = %d after a second purge, want %d", got, sentinelBytes)
	}

	// Deleting the clip afterwards returns only the sentinel; the finished
	// session's bytes are never handed back twice.
	requireStatus(t, do(t, app, http.MethodDelete, "/api/clips/"+clipID+"?environmentId="+env, nil), http.StatusOK)
	if got := reservedValue(t, app); got != sentinelBytes {
		t.Fatalf("deleting the clip moved the upload ledger: reserved = %d", got)
	}
	requireStatus(t, do(t, app, http.MethodDelete, "/api/uploads/"+sentinelID, nil), http.StatusOK)
	if got := reservedValue(t, app); got != 0 {
		t.Fatalf("reserved = %d after aborting the sentinel, want 0", got)
	}
}
