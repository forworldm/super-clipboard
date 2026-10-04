package api

// End-to-end chunked-upload scenarios that cannot be covered by unit tests:
//   - resume across a full server restart ("断电重启")
//   - chunked-transfer (no Content-Length) streaming overflow
//   - Content-Length header lying about the real body size
//   - concurrent completes: exactly one clip, idempotent/or-conflict losers
//   - concurrent DELETE vs complete race: no orphan files, no duplicate clips

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/pixia1234/super-clipboard/backend/internal/config"
	"github.com/pixia1234/super-clipboard/backend/internal/repository"
	"github.com/pixia1234/super-clipboard/backend/internal/storage"
)

// newTestAppOnDir builds an app rooted at an explicit dir so several
// "server generations" can share one storage/database location.
func newTestAppOnDir(t *testing.T, dir string, mutate func(*config.Settings)) (*App, *repository.ClipRepository) {
	t.Helper()
	settings := config.Defaults()
	settings.DatabasePath = filepath.Join(dir, "clips.db")
	settings.FileStorageDir = filepath.Join(dir, "files")
	settings.StaticRoot = filepath.Join(dir, "static")
	settings.CleanupIntervalSeconds = 3600
	settings.UploadChunkSizeBytes = 64 << 10
	settings.UploadSessionTTLSeconds = 3600
	if mutate != nil {
		mutate(settings)
	}
	repo, err := repository.NewClipRepository(settings)
	if err != nil {
		t.Fatalf("unable to open repository: %v", err)
	}
	app := NewApp(settings, repo)
	app.logger = log.New(io.Discard, "", 0)
	return app, repo
}

// unknownLengthReader hides the body size from httptest so the outgoing
// request has ContentLength == -1 (chunked transfer-encoding in the real world).
type unknownLengthReader struct{ inner io.Reader }

func (u *unknownLengthReader) Read(p []byte) (int, error) { return u.inner.Read(p) }
func (u *unknownLengthReader) Close() error               { return nil }

// TestResumeAcrossServerRestart: upload 1/2 chunks against generation 1,
// "restart the server" (new repo + app on the same dirs), then resume and
// complete. Session rows and chunk receipts must survive the restart.
func TestResumeAcrossServerRestart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// --- generation 1: init + partial upload ---
	app1, repo1 := newTestAppOnDir(t, dir, nil)
	chunkSize := 64 << 10
	data := deterministicBytes(chunkSize + 12345)
	init := initClipUpload(t, app1, "restart.bin", int64(len(data)), "application/octet-stream", "env-rs", map[string]interface{}{
		"expiresAt": futureTimestamp(2), "maxDownloads": 3,
	})
	uploadID := init["uploadId"].(string)
	if int(init["totalChunks"].(float64)) != 2 {
		t.Fatalf("expected 2 chunks, got %v", init["totalChunks"])
	}
	requireStatus(t, putChunk(t, app1, uploadID, 0, sliceFor(chunkSize, data, 0)), http.StatusOK)
	// "Power off": close the DB handle (files stay on disk).
	if err := repo1.Close(); err != nil {
		t.Fatalf("close repo1: %v", err)
	}

	// --- generation 2: cold start on the same storage ---
	app2, repo2 := newTestAppOnDir(t, dir, nil)
	t.Cleanup(func() { _ = repo2.Close() })
	// Startup recovery must not destroy the pending upload...
	app2.ReconcileUploadsOnStartup()

	// Resume info still reports chunk 0 as received.
	info := decode(t, getUpload(t, app2, uploadID))
	if info["receivedCount"] != float64(1) {
		t.Fatalf("restart lost progress: %v", info)
	}
	missing := info["missingChunks"].([]interface{})
	if len(missing) != 1 || missing[0] != float64(1) {
		t.Fatalf("restart lost progress, missing=%v", missing)
	}
	// The bytes written before the restart are still in their final position.
	session2, _ := app2.Repo.GetUploadSession(uploadID)
	if session2 == nil || session2.StagedPath == "" {
		t.Fatalf("session must keep its pre-allocated file: %+v", session2)
	}
	if got, err := os.ReadFile(session2.StagedPath); err != nil ||
		!bytes.Equal(got[:chunkSize], sliceFor(chunkSize, data, 0)) {
		t.Fatalf("chunk bytes lost across restart: %v", err)
	}

	// Idempotent init replay (same requestId was never used here, so a fresh
	// init would create a NEW session — resume continues on the existing one).
	requireStatus(t, putChunk(t, app2, uploadID, 1, sliceFor(chunkSize, data, 1)), http.StatusOK)
	done := completeUpload(t, app2, uploadID, nil)
	requireStatus(t, done, http.StatusCreated)
	clip := decode(t, done)
	clipID := clip["id"].(string)

	// The assembled clip downloads identically.
	dl := do(t, app2, http.MethodGet, "/api/clips/"+clipID+"/file?environmentId=env-rs", nil)
	requireStatus(t, dl, http.StatusOK)
	if !bytes.Equal(dl.Body.Bytes(), data) {
		t.Fatalf("post-restart clip content mismatch")
	}
	// Exactly one clip for this env.
	listed := decode(t, do(t, app2, http.MethodGet, "/api/clips?environmentId=env-rs", nil))
	if len(listed["items"].([]interface{})) != 1 {
		t.Fatalf("expected exactly one clip, got %v", listed)
	}
	// Idempotent complete replay after restart returns the same clip.
	replay := completeUpload(t, app2, uploadID, nil)
	requireStatus(t, replay, http.StatusOK)
	if decode(t, replay)["id"] != clipID {
		t.Fatalf("replay returned a different clip: %s", replay.Body.String())
	}
}

// TestChunkedTransferOverflowIs413NoDisk: no Content-Length header, body
// larger than the expected chunk -> streaming guard returns 413, the surplus
// byte is never written and no receipt is recorded (nothing to clean up either,
// since there is no temp file to leak).
func TestChunkedTransferOverflowIs413NoDisk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	app, repo := newTestAppOnDir(t, dir, nil)
	t.Cleanup(func() { _ = repo.Close() })

	chunkSize := 64 << 10
	init := initUpload(t, app, "overflow.bin", int64(chunkSize), "application/octet-stream", "env-ovf")
	uploadID := init["uploadId"].(string)

	oversized := deterministicBytes(chunkSize + 4096)
	req := httptest.NewRequest(http.MethodPut,
		fmt.Sprintf("/api/uploads/%s/chunks/0", uploadID),
		&unknownLengthReader{inner: bytes.NewReader(oversized)})
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = -1 // chunked / unknown
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusRequestEntityTooLarge)
	if !strings.Contains(rec.Body.String(), "分片体积超过限制") {
		t.Fatalf("expected 413 body, got %s", rec.Body.String())
	}
	// Nothing is marked received and no artifact is left behind: the storage dir
	// holds only the pre-allocated container (no temp file, no per-chunk file of
	// the legacy layout), and the surplus byte never left the request body.
	session, _ := app.Repo.GetUploadSession(uploadID)
	if session == nil || session.StagedPath == "" {
		t.Fatalf("session should own its pre-allocated file: %+v", session)
	}
	if names := storageFileNames(t, app); len(names) != 1 || names[0] != filepath.Base(session.StagedPath) {
		t.Fatalf("storage dir must hold exactly the pre-allocated file, got %v", names)
	}
	if info, err := os.Stat(session.StagedPath); err != nil || info.Size() != int64(chunkSize) {
		t.Fatalf("container must keep its declared size: %v %v", info, err)
	}
	if entries, err := os.ReadDir(storage.LegacyUploadRoot(app.Settings.FileStorageDir)); err == nil && len(entries) > 0 {
		t.Fatalf("legacy chunk layout must not be used anymore, found %d entries", len(entries))
	}
	// No DB receipt was recorded.
	info := decode(t, getUpload(t, app, uploadID))
	if info["receivedCount"] != float64(0) {
		t.Fatalf("no receipt may exist after overflow, got %v", info)
	}
	// Session still usable: resend the correct chunk size and complete.
	requireStatus(t, putChunk(t, app, uploadID, 0, deterministicBytes(chunkSize)), http.StatusOK)
	done := completeUpload(t, app, uploadID, nil)
	requireStatus(t, done, http.StatusCreated)
	if decode(t, done)["type"] != "file" {
		t.Fatalf("complete must insert a file clip, got %s", done.Body.String())
	}
}

// TestContentLengthMismatch: a lying Content-Length must never be recorded as a
// received chunk and the session must stay resumable.
func TestContentLengthMismatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	app, repo := newTestAppOnDir(t, dir, nil)
	t.Cleanup(func() { _ = repo.Close() })
	chunkSize := 64 << 10

	// Case 1: declared > actual. The drain short-circuits mid-body... the
	// stored payload is undersize -> 400 and nothing on disk.
	init := initUpload(t, app, "mismatch1.bin", int64(chunkSize), "application/octet-stream", "env-mm")
	u1 := init["uploadId"].(string)
	small := deterministicBytes(1024)
	req := httptest.NewRequest(http.MethodPut, "/api/uploads/"+u1+"/chunks/0", bytes.NewReader(small))
	req.ContentLength = int64(chunkSize) // lies: body is only 1024b
	req.Header.Set("Content-Type", "application/octet-stream")
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	requireStatus(t, rec, http.StatusBadRequest)
	if !strings.Contains(rec.Body.String(), "分片大小不匹配") {
		t.Fatalf("expected size-mismatch 400, got %s", rec.Body.String())
	}
	// The range may not be advertised as complete: a truncated body is refused
	// without a receipt (the partial prefix is harmless -- the retry rewrites the
	// whole range), and only the container exists on disk.
	if names := storageFileNames(t, app); len(names) != 1 {
		t.Fatalf("exactly one pre-allocated container expected, got %v", names)
	}
	if info, err := os.Stat(sessionPath(t, app, u1)); err != nil || info.Size() != int64(chunkSize) {
		t.Fatalf("container must keep its declared size: %v %v", info, err)
	}
	if info := decode(t, getUpload(t, app, u1)); info["receivedCount"] != float64(0) {
		t.Fatalf("no receipt expected, got %v", info)
	}
	// Session still active (no completing/completed state).
	if s, _ := app.Repo.GetUploadSession(u1); s.Status != "active" {
		t.Fatalf("session must stay active, got %s", s.Status)
	}
	// Retry with the honest body size succeeds.
	requireStatus(t, putChunk(t, app, u1, 0, deterministicBytes(chunkSize)), http.StatusOK)
	requireStatus(t, completeUpload(t, app, u1, nil), http.StatusCreated)

	// Case 2: declared < actual. A declared length that disagrees with the byte
	// range is a parameter error (400) and is answered before any byte is written
	// -- the undeclared (chunked) overflow path is covered by
	// TestChunkedTransferOverflowIs413NoDisk, which streams without a length.
	init2 := initUpload(t, app, "mismatch2.bin", int64(chunkSize), "application/octet-stream", "env-mm2")
	u2 := init2["uploadId"].(string)
	big := deterministicBytes(chunkSize + 8192)
	req2 := httptest.NewRequest(http.MethodPut, "/api/uploads/"+u2+"/chunks/0", bytes.NewReader(big))
	req2.ContentLength = 1234 // lies: body is far larger than declared
	req2.Header.Set("Content-Type", "application/octet-stream")
	rec2 := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec2, req2)
	requireStatus(t, rec2, http.StatusBadRequest)
	if !strings.Contains(rec2.Body.String(), "分片大小不匹配") {
		t.Fatalf("expected size-mismatch 400, got %s", rec2.Body.String())
	}
	if stored, _ := os.ReadFile(sessionPath(t, app, u2)); !bytes.Equal(stored, make([]byte, len(stored))) {
		t.Fatalf("a declared-size mismatch must not write any byte")
	}
	// An oversize body is refused without a receipt: the range is not marked
	// complete (a retry rewrites it in full), and no artifact of the old layout
	// (temp file / per-chunk file) exists.
	if names := storageFileNames(t, app); len(names) != 2 {
		t.Fatalf("exactly two pre-allocated containers expected, got %v", names)
	}
	if received, _ := app.Repo.ListReceivedChunks(u2); len(received) != 0 {
		t.Fatalf("no receipt may be recorded for an oversize body, got %v", received)
	}
	if info := decode(t, getUpload(t, app, u2)); info["receivedCount"] != float64(0) {
		t.Fatalf("no receipt expected after overflow, got %v", info)
	}
	// Honest retry works.
	requireStatus(t, putChunk(t, app, u2, 0, deterministicBytes(chunkSize)), http.StatusOK)
	requireStatus(t, completeUpload(t, app, u2, nil), http.StatusCreated)
}

// TestConcurrentCompleteExactlyOnce: N clients POST /complete on the same
// fully-uploaded session; exactly one clip must exist afterwards. Winners see
// 201, losers get the idempotent replay (200) or the merge-in-flight conflict
// (409); nobody creates a second clip.
func TestConcurrentCompleteExactlyOnce(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	app, repo := newTestAppOnDir(t, dir, nil)
	t.Cleanup(func() { _ = repo.Close() })
	chunkSize := 64 << 10
	data := deterministicBytes(chunkSize + 4242)

	init := initClipUpload(t, app, "race-complete.bin", int64(len(data)), "application/octet-stream", "env-rc", map[string]interface{}{
		"expiresAt": futureTimestamp(2), "maxDownloads": 5,
	})
	uploadID := init["uploadId"].(string)
	for i := 0; i < 2; i++ {
		requireStatus(t, putChunk(t, app, uploadID, i, sliceFor(chunkSize, data, i)), http.StatusOK)
	}

	const racers = 8
	type outcome struct {
		code int
		body map[string]interface{}
	}
	results := make(chan outcome, racers)
	var wg sync.WaitGroup
	barrier := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-barrier
			rec := completeUpload(t, app, uploadID, nil)
			results <- outcome{code: rec.Code, body: decode(t, rec)}
		}()
	}
	close(barrier)
	wg.Wait()
	close(results)

	created := 0
	conflicts := 0
	replays := 0
	clipIDs := map[string]int{}
	for r := range results {
		switch r.code {
		case http.StatusCreated:
			created++
			if id, ok := r.body["id"].(string); ok {
				clipIDs[id]++
			}
		case http.StatusOK:
			replays++
			if id, ok := r.body["id"].(string); ok {
				clipIDs[id]++
			}
		case http.StatusConflict:
			conflicts++ // merge-in-flight: safe, idempotent by retry
		default:
			t.Fatalf("unexpected complete status %d (body: %v)", r.code, r.body)
		}
	}
	if created != 1 {
		t.Fatalf("exactly one create must succeed, got created=%d replays=%d conflicts=%d", created, replays, conflicts)
	}
	if len(clipIDs) != 1 {
		t.Fatalf("all successful responses must reference the same clip, got %v", clipIDs)
	}
	// Exactly one clip row exists for the environment.
	listed := decode(t, do(t, app, http.MethodGet, "/api/clips?environmentId=env-rc", nil))
	if got := len(listed["items"].([]interface{})); got != 1 {
		t.Fatalf("exactly one clip row expected, got %d", got)
	}
	// Session committed exactly once.
	s, _ := app.Repo.GetUploadSession(uploadID)
	if s == nil || s.Status != "completed" {
		t.Fatalf("session must be completed, got %+v", s)
	}
	for id := range clipIDs {
		if s.ClipID != id {
			t.Fatalf("session clip link mismatch: %s vs %s", s.ClipID, id)
		}
	}
	// A further complete is a pure idempotent replay.
	final := completeUpload(t, app, uploadID, nil)
	requireStatus(t, final, http.StatusOK)
	if decode(t, final)["id"] != s.ClipID {
		t.Fatalf("post-race replay must serve the stored clip")
	}
	// The clip owns exactly the session's container: no extra file was created
	// (no merge copy) and nothing was left behind.
	if names := storageFileNames(t, app); len(names) != 1 {
		t.Fatalf("complete must not create extra files, got %v", names)
	}
}

// TestConcurrentDeleteVsComplete: racing the cancel against the merge must
// never yield an orphan file or a duplicate/invalid final state. We retry the
// race several times to sample both interleavings.
func TestConcurrentDeleteVsComplete(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	app, repo := newTestAppOnDir(t, dir, nil)
	t.Cleanup(func() { _ = repo.Close() })
	chunkSize := 64 << 10

	for round := 0; round < 6; round++ {
		env := fmt.Sprintf("env-rd-%d", round)
		data := deterministicBytes(chunkSize + 777 + round)
		baselineFiles := len(storageFileNames(t, app))
		initUploadID := func() string {
			rec := initClipUpload(t, app, fmt.Sprintf("rd-%d.bin", round), int64(len(data)), "application/octet-stream", env, map[string]interface{}{
				"expiresAt": futureTimestamp(1),
			})
			return rec["uploadId"].(string)
		}
		uploadID := initUploadID()
		for i := 0; i < 2; i++ {
			requireStatus(t, putChunk(t, app, uploadID, i, sliceFor(chunkSize, data, i)), http.StatusOK)
		}

		barrier := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		completeResult := make(chan *httptest.ResponseRecorder, 1)
		deleteResult := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			defer wg.Done()
			<-barrier
			completeResult <- completeUpload(t, app, uploadID, nil)
		}()
		go func() {
			defer wg.Done()
			<-barrier
			deleteResult <- do(t, app, http.MethodDelete, "/api/uploads/"+uploadID, nil)
		}()
		close(barrier)
		wg.Wait()
		cr, dr := <-completeResult, <-deleteResult

		// White-list of legal outcomes:
		//   a) cancel won   -> DELETE 200, COMPLETE 404 (rows already gone)
		//   b) merge won    -> COMPLETE 201, DELETE 409 (completed, cannot cancel)
		//   c) mid-merge    -> DELETE 409 (completing), COMPLETE 201
		legal := (cr.Code == http.StatusNotFound && dr.Code == http.StatusOK) ||
			(cr.Code == http.StatusCreated && dr.Code == http.StatusConflict)
		if !legal {
			t.Fatalf("round %d: illegal race outcome: complete=%d delete=%d (complete body %s; delete body %s)",
				round, cr.Code, dr.Code, cr.Body.String(), dr.Body.String())
		}

		// Invariant 1: at most one clip for this env.
		listed := decode(t, do(t, app, http.MethodGet, "/api/clips?environmentId="+env, nil))
		items := listed["items"].([]interface{})
		if len(items) > 1 {
			t.Fatalf("round %d: duplicate clips after race: %v", round, listed)
		}

		// Invariant 2: exactly one container per round -- owned by the clip when
		// complete won, deleted when the cancel won. No merge copy appears.
		wanted := baselineFiles
		if cr.Code == http.StatusCreated {
			wanted = baselineFiles + 1
		}
		if names := storageFileNames(t, app); len(names) != wanted {
			t.Fatalf("round %d: expected %d storage file(s), got %v", round, wanted, names)
		}
		session, _ := app.Repo.GetUploadSession(uploadID)
		if cr.Code == http.StatusCreated {
			// Merge won: session completed and linked to exactly the one clip.
			if session == nil || session.Status != "completed" || session.ClipID == "" {
				t.Fatalf("round %d: completed session must link its clip, got %+v", round, session)
			}
			clip := items[0].(map[string]interface{})
			if clip["id"] != session.ClipID {
				t.Fatalf("round %d: clip link mismatch", round)
			}
			// The stored file exists and round-trips content.
			filePayload := clip["payload"].(map[string]interface{})["file"].(map[string]interface{})
			dl := do(t, app, http.MethodGet, "/api/clips/"+session.ClipID+"/file?environmentId="+env, nil)
			requireStatus(t, dl, http.StatusOK)
			if !bytes.Equal(dl.Body.Bytes(), data) {
				t.Fatalf("round %d: downloaded content mismatch (%v)", round, filePayload["name"])
			}
		} else {
			// Cancel won: no row remains and no clip exists.
			if session != nil {
				t.Fatalf("round %d: cancelled session must be gone, got %+v", round, session)
			}
			if len(items) != 0 {
				t.Fatalf("round %d: cancelled upload must not create clips", round)
			}
		}
		// Invariant 3: no stray .part files litter the storage root.
		rootEntries, _ := os.ReadDir(app.Settings.FileStorageDir)
		for _, e := range rootEntries {
			if !e.IsDir() && strings.Contains(e.Name(), ".part.") {
				t.Fatalf("round %d: orphan .part file %s", round, e.Name())
			}
		}

		// Invariant 4: every leftover staged path recorded in DB exists on disk.
		if session != nil && session.StagedPath != "" && session.ClipID == "" {
			if _, err := os.Stat(session.StagedPath); err != nil {
				t.Fatalf("round %d: staged path %s missing on disk: %v", round, session.StagedPath, err)
			}
		}
	}
}

// TestCompleteIdempotencyAfterLostResponse: the client retries the complete
// call after a successful-but-unread response (client timeout). The second
// call must be a pure replay that creates no duplicate clip.
func TestCompleteIdempotencyAfterLostResponse(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	app, repo := newTestAppOnDir(t, dir, nil)
	t.Cleanup(func() { _ = repo.Close() })
	chunkSize := 64 << 10
	data := deterministicBytes(chunkSize + 999)

	init := initClipUpload(t, app, "lost-resp.bin", int64(len(data)), "application/octet-stream", "env-lr", map[string]interface{}{
		"expiresAt": futureTimestamp(1),
	})
	uploadID := init["uploadId"].(string)
	for i := 0; i < 2; i++ {
		requireStatus(t, putChunk(t, app, uploadID, i, sliceFor(chunkSize, data, i)), http.StatusOK)
	}
	first := completeUpload(t, app, uploadID, nil)
	requireStatus(t, first, http.StatusCreated)
	clipID := decode(t, first)["id"].(string)

	// Re-drive the whole retry ladder the frontend performs after a timeout:
	// GET (resume) -> complete again.
	info := decode(t, getUpload(t, app, uploadID))
	if info["status"] != "completed" {
		t.Fatalf("GET should report completed, got %v", info)
	}
	second := completeUpload(t, app, uploadID, nil)
	requireStatus(t, second, http.StatusOK)
	if decode(t, second)["id"] != clipID {
		t.Fatalf("replay must return the original clip %s", clipID)
	}
	third := completeUpload(t, app, uploadID, nil) // even a file-only retry replays the clip
	requireStatus(t, third, http.StatusOK)
	if decode(t, third)["id"] != clipID {
		t.Fatalf("file-only replay must still return the original clip")
	}
	listed := decode(t, do(t, app, http.MethodGet, "/api/clips?environmentId=env-lr", nil))
	if got := len(listed["items"].([]interface{})); got != 1 {
		t.Fatalf("exactly one clip expected, got %d", got)
	}
}
