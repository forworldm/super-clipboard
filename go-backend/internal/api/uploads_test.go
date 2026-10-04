package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pixia1234/super-clipboard/backend/internal/config"
	"github.com/pixia1234/super-clipboard/backend/internal/storage"
)

func smallChunkSettings() func(*config.Settings) {
	return func(s *config.Settings) {
		s.UploadChunkSizeBytes = 64 << 10 // min allowed (Effective clamps smaller)
		s.UploadSessionTTLSeconds = 3600
		s.CleanupIntervalSeconds = 3600
	}
}

// doRaw performs a request with a raw (non-JSON) body, e.g. chunk bytes.
func doRaw(t *testing.T, app *App, method, target string, body []byte, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	return rec
}

// initUpload performs a minimal init: every upload is a clip, so environmentId
// and expiresAt are always present (only the "clip dressing" -- accessCode,
// accessToken, maxDownloads -- is optional).
func initUpload(t *testing.T, app *App, filename string, fileSize int64, mime, env string) map[string]interface{} {
	t.Helper()
	return initUploadBody(t, app, map[string]interface{}{
		"filename": filename, "fileSize": fileSize, "mimeType": mime, "environmentId": env,
		"expiresAt": futureTimestamp(2),
	})
}

func initClipUpload(t *testing.T, app *App, filename string, fileSize int64, mime, env string, extra map[string]interface{}) map[string]interface{} {
	t.Helper()
	body := map[string]interface{}{
		"filename": filename, "fileSize": fileSize, "mimeType": mime, "environmentId": env,
		"expiresAt": futureTimestamp(2),
	}
	for k, v := range extra {
		body[k] = v
	}
	return initUploadBody(t, app, body)
}

func initUploadBody(t *testing.T, app *App, body map[string]interface{}) map[string]interface{} {
	t.Helper()
	rec := do(t, app, http.MethodPost, "/api/uploads/init", body)
	requireStatus(t, rec, http.StatusCreated)
	return decode(t, rec)
}

func putChunk(t *testing.T, app *App, uploadID string, index int, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	return doRaw(t, app, http.MethodPut,
		fmt.Sprintf("/api/uploads/%s/chunks/%d", uploadID, index),
		data, "application/octet-stream")
}

func getUpload(t *testing.T, app *App, uploadID string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, app, http.MethodGet, "/api/uploads/"+uploadID, nil)
}

func completeUpload(t *testing.T, app *App, uploadID string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	if body == nil {
		// File-only: empty body (no JSON wrapper).
		req := httptest.NewRequest(http.MethodPost, "/api/uploads/"+uploadID+"/complete", nil)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}
	return do(t, app, http.MethodPost, "/api/uploads/"+uploadID+"/complete", body)
}

func deterministicBytes(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte((i*31 + 7) % 251)
	}
	return out
}

func sliceFor(chunkSize int, data []byte, index int) []byte {
	start := index * chunkSize
	if start >= len(data) {
		return nil
	}
	end := start + chunkSize
	if end > len(data) {
		end = len(data)
	}
	return data[start:end]
}

// TestUploadInitValidation covers 422/400 paths + server-generated knobs.
func TestUploadInitValidation(t *testing.T) {
	app := newTestApp(t, smallChunkSettings())

	// Missing filename -> 422.
	rec := do(t, app, http.MethodPost, "/api/uploads/init", map[string]interface{}{"fileSize": 10})
	requireStatus(t, rec, http.StatusUnprocessableEntity)

	// Negative size -> 422.
	rec = do(t, app, http.MethodPost, "/api/uploads/init", map[string]interface{}{"filename": "a", "fileSize": -1})
	requireStatus(t, rec, http.StatusUnprocessableEntity)

	// Too large -> 400 (needs Settings, not 422).
	rec = do(t, app, http.MethodPost, "/api/uploads/init", map[string]interface{}{
		"filename": "big.bin", "fileSize": app.Settings.MaxFileSizeBytes + 1,
		"environmentId": "env-1", "expiresAt": futureTimestamp(1),
	})
	requireStatus(t, rec, http.StatusBadRequest)
	if !strings.Contains(rec.Body.String(), "文件体积超过限制") {
		t.Fatalf("unexpected body %s", rec.Body.String())
	}

	// Client knobs are ignored: server generates uploadId/chunkSize.
	rec = do(t, app, http.MethodPost, "/api/uploads/init", map[string]interface{}{
		"filename": "a.bin", "fileSize": 100, "chunkSize": 123, "uploadId": "hacked", "totalChunks": 99,
		"environmentId": "env-1", "expiresAt": futureTimestamp(1),
	})
	requireStatus(t, rec, http.StatusCreated)
	payload := decode(t, rec)
	if payload["uploadId"] == "hacked" {
		t.Fatalf("uploadId must be server-generated, got hacked")
	}
	if payload["chunkSize"] == float64(123) {
		t.Fatalf("chunkSize must be server-generated")
	}
	if payload["chunkSize"] != float64(64<<10) {
		t.Fatalf("expected 64KiB chunks, got %v", payload["chunkSize"])
	}
	if payload["totalChunks"] != float64(1) {
		t.Fatalf("100 bytes in 64KiB chunks should be 1 chunk, got %v", payload["totalChunks"])
	}

	// Clip params are validated at init (not complete).
	rec = do(t, app, http.MethodPost, "/api/uploads/init", map[string]interface{}{
		"filename": "a.bin", "fileSize": 100, "environmentId": "env-1",
		"expiresAt": -1,
	})
	requireStatus(t, rec, http.StatusUnprocessableEntity)
	rec = do(t, app, http.MethodPost, "/api/uploads/init", map[string]interface{}{
		"filename": "a.bin", "fileSize": 100, "environmentId": "env-1",
		"expiresAt": futureTimestamp(1), "accessCode": "ab-cd",
	})
	requireStatus(t, rec, http.StatusUnprocessableEntity)
	rec = do(t, app, http.MethodPost, "/api/uploads/init", map[string]interface{}{
		"filename": "a.bin", "fileSize": 100, "expiresAt": futureTimestamp(1),
	})
	requireStatus(t, rec, http.StatusUnprocessableEntity)
	rec = do(t, app, http.MethodPost, "/api/uploads/init", map[string]interface{}{
		"filename": "a.bin", "fileSize": 100, "environmentId": "env-1",
		"expiresAt": 1, // past
	})
	requireStatus(t, rec, http.StatusBadRequest)
	if !strings.Contains(rec.Body.String(), "过期时间必须晚于当前时间") {
		t.Fatalf("past expiresAt should be rejected at init, got %s", rec.Body.String())
	}
}

// TestUploadInitRequiresEnvironmentID: a file clip without an owner is
// unreachable (no accessCode/accessToken -> only the env owner can list it), so
// init MUST refuse an upload that carries no environmentId.
func TestUploadInitRequiresEnvironmentID(t *testing.T) {
	app := newTestApp(t, smallChunkSettings())

	// Key absent -> 422 and nothing persisted (no row, no chunk dir).
	rec := do(t, app, http.MethodPost, "/api/uploads/init", map[string]interface{}{
		"filename": "ownerless.bin", "fileSize": 1000,
		"mimeType": "application/octet-stream", "expiresAt": futureTimestamp(1),
	})
	requireStatus(t, rec, http.StatusUnprocessableEntity)
	if !strings.Contains(rec.Body.String(), "environmentId") {
		t.Fatalf("error should mention environmentId, got %s", rec.Body.String())
	}

	// Blank/whitespace-only value -> 422 as well.
	rec = do(t, app, http.MethodPost, "/api/uploads/init", map[string]interface{}{
		"filename": "ownerless.bin", "fileSize": 1000, "environmentId": "   ",
		"expiresAt": futureTimestamp(1),
	})
	requireStatus(t, rec, http.StatusUnprocessableEntity)
	if !strings.Contains(rec.Body.String(), "environmentId") {
		t.Fatalf("blank environmentId should mention environmentId, got %s", rec.Body.String())
	}

	// Even with a valid accessCode / accessToken the owner is required.
	rec = do(t, app, http.MethodPost, "/api/uploads/init", map[string]interface{}{
		"filename": "ownerless.bin", "fileSize": 1000, "expiresAt": futureTimestamp(1),
		"accessCode": "12345",
	})
	requireStatus(t, rec, http.StatusUnprocessableEntity)
	rec = do(t, app, http.MethodPost, "/api/uploads/init", map[string]interface{}{
		"filename": "ownerless.bin", "fileSize": 1000, "expiresAt": futureTimestamp(1),
		"accessToken": "tok-without-env",
	})
	requireStatus(t, rec, http.StatusUnprocessableEntity)

	if sessions, _ := app.Repo.ListUploadSessions(); len(sessions) != 0 {
		t.Fatalf("a refused init must not persist a session, got %v", sessions)
	}
	if dirs, _ := storage.ListUploadDirsOnDisk(app.Settings.FileStorageDir); len(dirs) != 0 {
		t.Fatalf("a refused init must not create a chunk dir, got %v", dirs)
	}
}

// TestUploadInitRequiresExpiresAt: expiresAt is a mandatory clip param (the
// clips table stores NOT NULL expires_at), so it is rejected up-front instead of
// failing after every chunk was uploaded.
func TestUploadInitRequiresExpiresAt(t *testing.T) {
	app := newTestApp(t, smallChunkSettings())
	rec := do(t, app, http.MethodPost, "/api/uploads/init", map[string]interface{}{
		"filename": "no-expiry.bin", "fileSize": 1000, "environmentId": "env-noexp",
	})
	requireStatus(t, rec, http.StatusUnprocessableEntity)
	if sessions, _ := app.Repo.ListUploadSessions(); len(sessions) != 0 {
		t.Fatalf("a refused init must not persist a session, got %v", sessions)
	}
}

// TestFullChunkedUploadCreatesNamelessClip covers
// init->chunks(out-of-order)->info->complete for a NAMELESS clip: no
// accessCode and no accessToken, so the clip is reachable only through its
// environment owner. COMPLETE must still insert a clip (there is no
// regular-file mode).
func TestFullChunkedUploadCreatesNamelessClip(t *testing.T) {
	app := newTestApp(t, smallChunkSettings())
	chunkSize := 64 << 10
	fileSize := chunkSize*2 + 12345
	data := deterministicBytes(fileSize)
	env := "env-nameless-full"

	init := initUpload(t, app, "photo.png", int64(fileSize), "image/png", env)
	uploadID := init["uploadId"].(string)
	if int(init["totalChunks"].(float64)) != 3 {
		t.Fatalf("expected 3 chunks, got %v", init["totalChunks"])
	}

	// Upload out of order: 2, 0 (skip 1 for resume check).
	requireStatus(t, putChunk(t, app, uploadID, 2, sliceFor(chunkSize, data, 2)), http.StatusOK)
	requireStatus(t, putChunk(t, app, uploadID, 0, sliceFor(chunkSize, data, 0)), http.StatusOK)

	// Resume info must report exactly the missing chunk.
	infoRec := getUpload(t, app, uploadID)
	requireStatus(t, infoRec, http.StatusOK)
	info := decode(t, infoRec)
	if info["receivedCount"] != float64(2) {
		t.Fatalf("expected 2 received, got %v", info)
	}
	missing := info["missingChunks"].([]interface{})
	if len(missing) != 1 || missing[0] != float64(1) {
		t.Fatalf("missing should be [1], got %v", missing)
	}

	// Idempotent rewrite of chunk 0 (retry path).
	requireStatus(t, putChunk(t, app, uploadID, 0, sliceFor(chunkSize, data, 0)), http.StatusOK)

	// Complete with a gap must fail and stay resumable.
	early := completeUpload(t, app, uploadID, nil)
	requireStatus(t, early, http.StatusBadRequest)
	if !strings.Contains(early.Body.String(), "分片缺失") {
		t.Fatalf("expected missing-chunk detail, got %s", early.Body.String())
	}

	// Fill the gap and complete: the response is a FILE CLIP (201), never a
	// plain "file-only" payload.
	requireStatus(t, putChunk(t, app, uploadID, 1, sliceFor(chunkSize, data, 1)), http.StatusOK)
	done := completeUpload(t, app, uploadID, nil)
	requireStatus(t, done, http.StatusCreated)
	clip := decode(t, done)
	if clip["type"] != "file" {
		t.Fatalf("complete must return a file clip, got %v", clip)
	}
	if clip["accessCode"] != nil || clip["accessToken"] != nil {
		t.Fatalf("nameless clip must not expose access fields, got %v", clip)
	}
	clipID := clip["id"].(string)

	// Assembled bytes must match exactly; chunk dir must be cleaned.
	session, _ := app.Repo.GetUploadSession(uploadID)
	if session == nil || session.StagedPath == "" {
		t.Fatalf("session should record staged path: %+v", session)
	}
	if session.ClipID != clipID {
		t.Fatalf("session must link the created clip, got %q want %q", session.ClipID, clipID)
	}
	assembled, err := os.ReadFile(session.StagedPath)
	if err != nil {
		t.Fatalf("read staged: %v", err)
	}
	if !bytes.Equal(assembled, data) {
		t.Fatalf("assembled bytes mismatch (got %d want %d)", len(assembled), len(data))
	}
	if _, err := os.Stat(storage.UploadSessionDir(app.Settings.FileStorageDir, uploadID)); !os.IsNotExist(err) {
		t.Fatalf("chunk dir should be removed after complete")
	}

	// The clip is listed for its environment owner (the only way a nameless
	// clip can be discovered) and downloads byte-identically.
	listed := decode(t, do(t, app, http.MethodGet, "/api/clips?environmentId="+env, nil))
	items := listed["items"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("expected the nameless clip in its environment list, got %v", listed)
	}
	dl := do(t, app, http.MethodGet, "/api/clips/"+clipID+"/file?environmentId="+env, nil)
	requireStatus(t, dl, http.StatusOK)
	if !bytes.Equal(dl.Body.Bytes(), data) {
		t.Fatalf("download mismatch (got %d want %d)", dl.Body.Len(), len(data))
	}
	// Another environment cannot see it.
	other := decode(t, do(t, app, http.MethodGet, "/api/clips?environmentId=env-someone-else", nil))
	if len(other["items"].([]interface{})) != 0 {
		t.Fatalf("foreign environment must not list the clip, got %v", other)
	}

	// Idempotent replay: second complete returns the same clip (no dup).
	replay := completeUpload(t, app, uploadID, nil)
	requireStatus(t, replay, http.StatusOK)
	if decode(t, replay)["id"] != clipID {
		t.Fatalf("replay should return same clip: %s", replay.Body.String())
	}
	if listedAfter := decode(t, do(t, app, http.MethodGet, "/api/clips?environmentId="+env, nil)); len(listedAfter["items"].([]interface{})) != 1 {
		t.Fatalf("replay must not create a second clip: %v", listedAfter)
	}

	// GET on completed reports completed (not 404).
	infoRec = getUpload(t, app, uploadID)
	requireStatus(t, infoRec, http.StatusOK)
	if decode(t, infoRec)["status"] != "completed" {
		t.Fatalf("expected completed status, got %s", infoRec.Body.String())
	}
}

// TestCompleteIgnoresClientParams: /complete carries no clip parameters; the
// values frozen at init win even if an old client sends a conflicting body.
func TestCompleteIgnoresClientParams(t *testing.T) {
	app := newTestApp(t, smallChunkSettings())
	env := "env-frozen"
	data := deterministicBytes(4096)
	init := initClipUpload(t, app, "frozen.bin", int64(len(data)), "application/octet-stream", env, map[string]interface{}{
		"maxDownloads": 7, "accessCode": "24680",
	})
	uploadID := init["uploadId"].(string)
	requireStatus(t, putChunk(t, app, uploadID, 0, data), http.StatusOK)

	// A legacy-looking body (different code/expiry/env) must be ignored.
	rec := completeUpload(t, app, uploadID, map[string]interface{}{
		"environmentId": "env-hijack", "expiresAt": futureTimestamp(1),
		"maxDownloads": 499, "accessCode": "13579",
	})
	requireStatus(t, rec, http.StatusCreated)
	clip := decode(t, rec)
	if clip["accessCode"] != "24680" {
		t.Fatalf("clip must keep the init accessCode, got %v", clip)
	}
	if clip["maxDownloads"] != float64(7) {
		t.Fatalf("clip must keep the init maxDownloads, got %v", clip["maxDownloads"])
	}
	session, _ := app.Repo.GetUploadSession(uploadID)
	if session.EnvironmentID != env {
		t.Fatalf("clip owner must come from init, got %q", session.EnvironmentID)
	}
}

// TestCompleteWithClipCreation covers the clip-mode complete path end-to-end.
func TestCompleteWithClipCreation(t *testing.T) {
	app := newTestApp(t, smallChunkSettings())
	chunkSize := 64 << 10
	data := deterministicBytes(chunkSize + 5000)
	env := "env-clip"
	init := initClipUpload(t, app, "report.pdf", int64(len(data)), "application/pdf", env, map[string]interface{}{
		"maxDownloads": 5, "accessCode": "98765",
	})
	uploadID := init["uploadId"].(string)
	for i := 0; i < 2; i++ {
		requireStatus(t, putChunk(t, app, uploadID, i, sliceFor(chunkSize, data, i)), http.StatusOK)
	}
	clipRec := completeUpload(t, app, uploadID, nil)
	requireStatus(t, clipRec, http.StatusCreated)
	clip := decode(t, clipRec)
	if clip["type"] != "file" {
		t.Fatalf("expected file clip, got %v", clip["type"])
	}
	clipID := clip["id"].(string)
	filePayload := clip["payload"].(map[string]interface{})["file"].(map[string]interface{})
	if filePayload["name"] != "report.pdf" || filePayload["size"] != float64(len(data)) {
		t.Fatalf("unexpected file payload %v", filePayload)
	}
	// Download round-trips the assembled bytes.
	dl := do(t, app, http.MethodGet, "/api/clips/"+clipID+"/file?environmentId="+env, nil)
	requireStatus(t, dl, http.StatusOK)
	if !bytes.Equal(dl.Body.Bytes(), data) {
		t.Fatalf("download mismatch (got %d want %d)", dl.Body.Len(), len(data))
	}
	// Session is linked to the clip.
	session, _ := app.Repo.GetUploadSession(uploadID)
	if session.Status != "completed" || session.ClipID != clipID {
		t.Fatalf("session should link clip, got %+v", session)
	}
	// Replay (empty complete body) returns the same clip (no duplicate creation).
	replay := completeUpload(t, app, uploadID, nil)
	requireStatus(t, replay, http.StatusOK)
	if decode(t, replay)["id"] != clipID {
		t.Fatalf("replay should return same clip, got %s", replay.Body.String())
	}
}

// TestInitRejectsTakenAccessCode: a code already in clips is refused at init
// so the client never uploads chunks that cannot become a clip.
func TestInitRejectsTakenAccessCode(t *testing.T) {
	app := newTestApp(t, smallChunkSettings())
	env := "env-dup-init"
	legacy := do(t, app, http.MethodPost, "/api/clips", map[string]interface{}{
		"type": "text", "expiresAt": futureTimestamp(1), "environmentId": env,
		"accessCode": "55555", "payload": map[string]interface{}{"text": "taken"},
	})
	requireStatus(t, legacy, http.StatusCreated)

	rec := do(t, app, http.MethodPost, "/api/uploads/init", map[string]interface{}{
		"filename": "dup.bin", "fileSize": 1000, "mimeType": "application/octet-stream",
		"environmentId": env, "expiresAt": futureTimestamp(1), "accessCode": "55555",
	})
	requireStatus(t, rec, http.StatusConflict)
	if !strings.Contains(rec.Body.String(), "直链码已存在") {
		t.Fatalf("unexpected body %s", rec.Body.String())
	}
	if sessions, _ := app.Repo.ListUploadSessions(); len(sessions) != 0 {
		t.Fatalf("failed init must not persist a session, got %v", sessions)
	}
}

// TestCompleteDuplicateCodeRollback: two sessions capture the same unused
// access code at init; the first complete wins, the second 409s and rolls back
// (session stays active, chunks intact, no staged orphan).
func TestCompleteDuplicateCodeRollback(t *testing.T) {
	app := newTestApp(t, smallChunkSettings())
	env := "env-dup"
	data := deterministicBytes(1000)

	init1 := initClipUpload(t, app, "dup1.bin", int64(len(data)), "application/octet-stream", env, map[string]interface{}{
		"accessCode": "55555", "expiresAt": futureTimestamp(1),
	})
	init2 := initClipUpload(t, app, "dup2.bin", int64(len(data)), "application/octet-stream", env, map[string]interface{}{
		"accessCode": "55555", "expiresAt": futureTimestamp(1),
	})
	id1 := init1["uploadId"].(string)
	id2 := init2["uploadId"].(string)
	requireStatus(t, putChunk(t, app, id1, 0, data), http.StatusOK)
	requireStatus(t, putChunk(t, app, id2, 0, data), http.StatusOK)

	first := completeUpload(t, app, id1, nil)
	requireStatus(t, first, http.StatusCreated)

	beforeEntries, _ := os.ReadDir(app.Settings.FileStorageDir)
	beforeNames := map[string]bool{}
	for _, e := range beforeEntries {
		beforeNames[e.Name()] = true
	}
	conflict := completeUpload(t, app, id2, nil)
	requireStatus(t, conflict, http.StatusConflict)
	if !strings.Contains(conflict.Body.String(), "直链码已存在") {
		t.Fatalf("unexpected body %s", conflict.Body.String())
	}
	// Rollback: session back to active, chunks intact, no staged orphan.
	session, _ := app.Repo.GetUploadSession(id2)
	if session.Status != "active" {
		t.Fatalf("session should roll back to active, got %s", session.Status)
	}
	if session.StagedPath != "" {
		t.Fatalf("rolled-back session must clear staged_path, got %q", session.StagedPath)
	}
	if _, err := os.Stat(storage.ChunkFilePath(app.Settings.FileStorageDir, id2, 0)); err != nil {
		t.Fatalf("chunk must survive rollback: %v", err)
	}
	afterEntries, _ := os.ReadDir(app.Settings.FileStorageDir)
	for _, e := range afterEntries {
		if e.IsDir() || beforeNames[e.Name()] {
			continue
		}
		t.Fatalf("staged orphan leaked: %s", e.Name())
	}
	// Retrying complete with the same stored code still conflicts (params are
	// frozen at init); the session stays resumable/abortable.
	again := completeUpload(t, app, id2, nil)
	requireStatus(t, again, http.StatusConflict)
}

// TestChunkSizeValidation covers wrong-size / oversize / out-of-range.
func TestChunkSizeValidation(t *testing.T) {
	app := newTestApp(t, smallChunkSettings())
	chunkSize := 64 << 10
	data := deterministicBytes(chunkSize + 100)
	init := initUpload(t, app, "s.bin", int64(len(data)), "application/octet-stream", "env-chunk")
	uploadID := init["uploadId"].(string)

	// Undersize first chunk (must be exactly chunkSize).
	rec := putChunk(t, app, uploadID, 0, data[:1000])
	requireStatus(t, rec, http.StatusBadRequest)
	if !strings.Contains(rec.Body.String(), "分片大小不匹配") {
		t.Fatalf("unexpected %s", rec.Body.String())
	}
	// Oversize chunk.
	rec = putChunk(t, app, uploadID, 0, deterministicBytes(chunkSize+1))
	if rec.Code != http.StatusRequestEntityTooLarge && rec.Code != http.StatusBadRequest {
		t.Fatalf("oversize should be 400/413, got %d (%s)", rec.Code, rec.Body.String())
	}
	// Out of range.
	rec = putChunk(t, app, uploadID, 5, []byte("x"))
	requireStatus(t, rec, http.StatusBadRequest)
	// Invalid index literal.
	bad := doRaw(t, app, http.MethodPut, "/api/uploads/"+uploadID+"/chunks/abc", []byte("x"), "application/octet-stream")
	requireStatus(t, bad, http.StatusBadRequest)
	// Session still active with nothing received.
	info := decode(t, getUpload(t, app, uploadID))
	if info["receivedCount"] != float64(0) {
		t.Fatalf("no chunks should be recorded, got %v", info)
	}
}

// TestAbortAndCleanup covers DELETE (cancel path) + idempotency.
func TestAbortAndCleanup(t *testing.T) {
	app := newTestApp(t, smallChunkSettings())
	data := deterministicBytes(5000)
	init := initUpload(t, app, "cancel.bin", int64(len(data)), "application/octet-stream", "env-abort")
	uploadID := init["uploadId"].(string)
	requireStatus(t, putChunk(t, app, uploadID, 0, data), http.StatusOK)

	del := do(t, app, http.MethodDelete, "/api/uploads/"+uploadID, nil)
	requireStatus(t, del, http.StatusOK)

	if rec := getUpload(t, app, uploadID); rec.Code != http.StatusNotFound {
		t.Fatalf("GET after abort should 404, got %d", rec.Code)
	}
	if _, err := os.Stat(storage.UploadSessionDir(app.Settings.FileStorageDir, uploadID)); !os.IsNotExist(err) {
		t.Fatalf("chunk dir should be removed")
	}
	if s, _ := app.Repo.GetUploadSession(uploadID); s != nil {
		t.Fatalf("DB row should be gone")
	}
	// Second DELETE is 404 (already gone).
	del2 := do(t, app, http.MethodDelete, "/api/uploads/"+uploadID, nil)
	requireStatus(t, del2, http.StatusNotFound)

	// Completed-with-clip cannot be aborted.
	init2 := initClipUpload(t, app, "c.bin", 10, "application/octet-stream", "env-abort", map[string]interface{}{
		"expiresAt": futureTimestamp(1),
	})
	upload2 := init2["uploadId"].(string)
	requireStatus(t, putChunk(t, app, upload2, 0, deterministicBytes(10)), http.StatusOK)
	clipRec := completeUpload(t, app, upload2, nil)
	requireStatus(t, clipRec, http.StatusCreated)
	del3 := do(t, app, http.MethodDelete, "/api/uploads/"+upload2, nil)
	requireStatus(t, del3, http.StatusConflict)
}

// TestExpiredSessionHandling covers timeout rollback (410 + cleanup).
func TestExpiredSessionHandling(t *testing.T) {
	app := newTestApp(t, smallChunkSettings())
	data := deterministicBytes(2000)
	init := initUpload(t, app, "exp.bin", int64(len(data)), "application/octet-stream", "env-expired")
	uploadID := init["uploadId"].(string)
	requireStatus(t, putChunk(t, app, uploadID, 0, data), http.StatusOK)
	// Force expiry.
	if err := app.Repo.SetUploadExpiryForTest(uploadID, time.Now().Add(-time.Minute).Unix()); err != nil {
		t.Fatalf("unable to expire session: %v", err)
	}
	if rec := getUpload(t, app, uploadID); rec.Code != http.StatusGone {
		t.Fatalf("GET expired should 410, got %d (%s)", rec.Code, rec.Body.String())
	}
	// PUT/complete after expiry are 404 (row already purged by the GET above).
	if rec := putChunk(t, app, uploadID, 0, data); rec.Code != http.StatusNotFound && rec.Code != http.StatusGone {
		t.Fatalf("PUT expired should 404/410, got %d", rec.Code)
	}
	if _, err := os.Stat(storage.UploadSessionDir(app.Settings.FileStorageDir, uploadID)); !os.IsNotExist(err) {
		t.Fatalf("expired chunk dir should be cleaned")
	}
}

// TestConcurrentChunkUploads proves parallel PUTs (no long global lock).
func TestConcurrentChunkUploads(t *testing.T) {
	app := newTestApp(t, smallChunkSettings())
	chunkSize := 64 << 10
	totalChunks := 5
	fileSize := chunkSize*(totalChunks-1) + 7777
	data := deterministicBytes(fileSize)
	init := initUpload(t, app, "conc.bin", int64(fileSize), "application/octet-stream", "env-conc")
	uploadID := init["uploadId"].(string)

	var wg sync.WaitGroup
	errCh := make(chan string, totalChunks)
	for i := 0; i < totalChunks; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			rec := putChunk(t, app, uploadID, index, sliceFor(chunkSize, data, index))
			if rec.Code != http.StatusOK {
				errCh <- fmt.Sprintf("chunk %d: %d %s", index, rec.Code, rec.Body.String())
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Fatalf("concurrent PUT failed: %s", e)
	}
	done := completeUpload(t, app, uploadID, nil)
	requireStatus(t, done, http.StatusCreated)
	if decode(t, done)["type"] != "file" {
		t.Fatalf("complete must insert a file clip, got %s", done.Body.String())
	}
	session, _ := app.Repo.GetUploadSession(uploadID)
	if session.ClipID == "" {
		t.Fatalf("session must link its clip, got %+v", session)
	}
	assembled, _ := os.ReadFile(session.StagedPath)
	if !bytes.Equal(assembled, data) {
		t.Fatalf("concurrent assembly mismatch")
	}
}

// TestCrashRecoveryResetStuck simulates power loss mid-assembly.
func TestCrashRecoveryResetStuck(t *testing.T) {
	app := newTestApp(t, smallChunkSettings())
	chunkSize := 64 << 10
	data := deterministicBytes(chunkSize + 11)
	init := initUpload(t, app, "crash.bin", int64(len(data)), "application/octet-stream", "env-crash")
	uploadID := init["uploadId"].(string)
	for i := 0; i < 2; i++ {
		requireStatus(t, putChunk(t, app, uploadID, i, sliceFor(chunkSize, data, i)), http.StatusOK)
	}
	// Simulate crash: flip to completing (as COMPLETE would) then "die" before commit.
	if _, _, err := app.Repo.TryBeginComplete(uploadID, filepath.Join(app.Settings.FileStorageDir, "crash-partial.bin")); err != nil {
		t.Fatalf("begin: %v", err)
	}
	// Leave a partial staged file + temp behind.
	_ = os.WriteFile(filepath.Join(app.Settings.FileStorageDir, "crash-partial.bin"), []byte("partial"), 0o644)
	_ = os.WriteFile(filepath.Join(app.Settings.FileStorageDir, "orphan.part.123"), []byte("t"), 0o644)
	// Orphan chunk dir with no DB row.
	_ = os.MkdirAll(storage.UploadSessionDir(app.Settings.FileStorageDir, "orphan-no-row"), 0o755)

	app.ReconcileUploadsOnStartup()

	session, _ := app.Repo.GetUploadSession(uploadID)
	if session.Status != "active" {
		t.Fatalf("stuck session should reset to active, got %s", session.Status)
	}
	if _, err := os.Stat(filepath.Join(app.Settings.FileStorageDir, "crash-partial.bin")); !os.IsNotExist(err) {
		t.Fatalf("partial staged file should be removed")
	}
	if _, err := os.Stat(filepath.Join(app.Settings.FileStorageDir, "orphan.part.123")); !os.IsNotExist(err) {
		t.Fatalf("orphan part temp should be swept")
	}
	if _, err := os.Stat(storage.UploadSessionDir(app.Settings.FileStorageDir, "orphan-no-row")); !os.IsNotExist(err) {
		t.Fatalf("orphan dir should be swept")
	}
	// Chunks survived -> complete now succeeds and inserts the clip.
	done := completeUpload(t, app, uploadID, nil)
	requireStatus(t, done, http.StatusCreated)
	if decode(t, done)["type"] != "file" {
		t.Fatalf("expected file clip after recovery, got %s", done.Body.String())
	}
	session2, _ := app.Repo.GetUploadSession(uploadID)
	assembled, _ := os.ReadFile(session2.StagedPath)
	if !bytes.Equal(assembled, data) {
		t.Fatalf("post-recovery assembly mismatch")
	}
}

// TestDiskLossReconcile covers chunk files deleted out-of-band.
func TestDiskLossReconcile(t *testing.T) {
	app := newTestApp(t, smallChunkSettings())
	chunkSize := 64 << 10
	data := deterministicBytes(chunkSize*2 + 100)
	init := initUpload(t, app, "loss.bin", int64(len(data)), "application/octet-stream", "env-loss")
	uploadID := init["uploadId"].(string)
	for i := 0; i < 3; i++ {
		requireStatus(t, putChunk(t, app, uploadID, i, sliceFor(chunkSize, data, i)), http.StatusOK)
	}
	// Simulate disk loss of chunk 1.
	if err := os.Remove(storage.ChunkFilePath(app.Settings.FileStorageDir, uploadID, 1)); err != nil {
		t.Fatalf("remove chunk: %v", err)
	}
	// GET reconciles and reports the gap.
	infoRec := getUpload(t, app, uploadID)
	requireStatus(t, infoRec, http.StatusOK)
	missing := decode(t, infoRec)["missingChunks"].([]interface{})
	if len(missing) != 1 || missing[0] != float64(1) {
		t.Fatalf("GET should report [1] missing, got %v", missing)
	}
	// Complete surfaces the same gap.
	early := completeUpload(t, app, uploadID, nil)
	requireStatus(t, early, http.StatusBadRequest)
	var body struct {
		Detail  string `json:"detail"`
		Missing []int  `json:"missing"`
	}
	// Note: success uses lowercase keys; error detail uses FastAPI "Detail".
	var raw map[string]interface{}
	if err := json.Unmarshal(early.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode early: %v", err)
	}
	_ = body
	if _, ok := raw["missing"]; !ok {
		t.Fatalf("complete should include missing list, got %v", raw)
	}
	// Re-upload the lost chunk -> complete succeeds (clip created).
	requireStatus(t, putChunk(t, app, uploadID, 1, sliceFor(chunkSize, data, 1)), http.StatusOK)
	requireStatus(t, completeUpload(t, app, uploadID, nil), http.StatusCreated)
}

// TestEmptyFileUpload covers 0-byte files (0 chunks, direct complete).
func TestEmptyFileUpload(t *testing.T) {
	app := newTestApp(t, smallChunkSettings())
	init := initClipUpload(t, app, "empty.txt", 0, "text/plain", "env-empty", map[string]interface{}{
		"expiresAt": futureTimestamp(1),
	})
	uploadID := init["uploadId"].(string)
	if init["totalChunks"] != float64(0) {
		t.Fatalf("empty file should have 0 chunks, got %v", init["totalChunks"])
	}
	if rec := putChunk(t, app, uploadID, 0, []byte{}); rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT on empty session should 400, got %d", rec.Code)
	}
	done := completeUpload(t, app, uploadID, nil)
	requireStatus(t, done, http.StatusCreated)
	clip := decode(t, done)
	size := clip["payload"].(map[string]interface{})["file"].(map[string]interface{})["size"]
	if size != float64(0) {
		t.Fatalf("empty clip size should be 0, got %v", size)
	}
}

// TestInvalidUploadIDs covers 404 paths.
func TestInvalidUploadIDs(t *testing.T) {
	app := newTestApp(t, smallChunkSettings())
	if rec := getUpload(t, app, "no-such-id"); rec.Code != http.StatusNotFound {
		t.Fatalf("GET unknown should 404, got %d", rec.Code)
	}
	if rec := putChunk(t, app, "no-such-id", 0, []byte("x")); rec.Code != http.StatusNotFound {
		t.Fatalf("PUT unknown should 404, got %d", rec.Code)
	}
	if rec := completeUpload(t, app, "no-such-id", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("COMPLETE unknown should 404, got %d", rec.Code)
	}
	del := do(t, app, http.MethodDelete, "/api/uploads/no-such-id", nil)
	requireStatus(t, del, http.StatusNotFound)
}

// TestCaptchaVerifiedAtInit proves captcha is checked BEFORE any chunk
// consumes storage (storage-DoS guard), not at complete.
func TestCaptchaVerifiedAtInit(t *testing.T) {
	app := newTestApp(t, func(s *config.Settings) {
		s.UploadChunkSizeBytes = 64 << 10
		s.CaptchaProvider = "turnstile"
		s.CaptchaSecret = "dummy-secret"
		s.CaptchaBypassToken = "pass-me"
		s.CaptchaSiteKey = "dummy-site-key"
	})

	// 1) init without captcha -> 400, NOTHING persisted (no row, no dir).
	bad := do(t, app, http.MethodPost, "/api/uploads/init", map[string]interface{}{
		"filename": "cap.bin", "fileSize": 1500, "environmentId": "env-cap",
		"expiresAt": futureTimestamp(1),
	})
	requireStatus(t, bad, http.StatusBadRequest)
	if !strings.Contains(bad.Body.String(), "缺少验证码") {
		t.Fatalf("unexpected body %s", bad.Body.String())
	}
	// No session row must exist: init returned no uploadId so nothing to query;
	// assert there are 0 upload dirs and 0 session rows.
	if dirs, _ := storage.ListUploadDirsOnDisk(app.Settings.FileStorageDir); len(dirs) != 0 {
		t.Fatalf("no upload dir may exist after failed init, got %v", dirs)
	}
	if sessions, _ := app.Repo.ListUploadSessions(); len(sessions) != 0 {
		t.Fatalf("no session row may exist after failed init, got %v", sessions)
	}

	// 2) init with captcha -> 201, chunks accepted, complete needs NO captcha
	// and no clip fields (they were captured at init).
	good := do(t, app, http.MethodPost, "/api/uploads/init", map[string]interface{}{
		"filename": "cap.bin", "fileSize": 1500, "environmentId": "env-cap",
		"expiresAt":    futureTimestamp(1),
		"captchaToken": "pass-me", "captchaProvider": "turnstile",
	})
	requireStatus(t, good, http.StatusCreated)
	uploadID := decode(t, good)["uploadId"].(string)
	data := deterministicBytes(1500)
	requireStatus(t, putChunk(t, app, uploadID, 0, data), http.StatusOK)
	done := completeUpload(t, app, uploadID, nil)
	requireStatus(t, done, http.StatusCreated)
	if decode(t, done)["type"] != "file" {
		t.Fatalf("expected file clip, got %s", done.Body.String())
	}
}

// TestInitIdempotentReplay_ReturnsSameSession: same (env, requestId) twice ->
// same uploadId, no second session/dir; captcha not re-required on replay.
func TestInitIdempotentReplay_ReturnsSameSession(t *testing.T) {
	app := newTestApp(t, func(s *config.Settings) {
		s.UploadChunkSizeBytes = 64 << 10
		s.CaptchaProvider = "turnstile"
		s.CaptchaSecret = "dummy-secret"
		s.CaptchaBypassToken = "pass-me"
		s.CaptchaSiteKey = "dummy-site-key"
	})
	first := do(t, app, http.MethodPost, "/api/uploads/init", map[string]interface{}{
		"filename": "idem.bin", "fileSize": 200000, "environmentId": "env-idem",
		"expiresAt": futureTimestamp(2),
		"requestId": "req-42", "captchaToken": "pass-me", "captchaProvider": "turnstile",
	})
	requireStatus(t, first, http.StatusCreated)
	a := decode(t, first)["uploadId"].(string)

	// Upload a chunk, then replay init with the SAME requestId but NO captcha.
	requireStatus(t, putChunk(t, app, a, 0, deterministicBytes(64<<10)), http.StatusOK)
	replay := do(t, app, http.MethodPost, "/api/uploads/init", map[string]interface{}{
		"filename": "idem.bin", "fileSize": 200000, "environmentId": "env-idem",
		"expiresAt": futureTimestamp(2),
		"requestId": "req-42",
	})
	requireStatus(t, replay, http.StatusOK)
	b := decode(t, replay)
	if b["uploadId"] != a {
		t.Fatalf("idempotent replay must return same uploadId: %s vs %s", a, b["uploadId"])
	}
	if b["status"] != "active" {
		t.Fatalf("replay of active session should be active, got %v", b["status"])
	}
	// And the chunk progress must be visible in the replay payload.
	received := b["receivedChunks"].([]interface{})
	if len(received) != 1 || received[0] != float64(0) {
		t.Fatalf("replay should expose received chunks, got %v", received)
	}

	// Only ONE session row exists.
	sessions, _ := app.Repo.ListUploadSessions()
	if len(sessions) != 1 {
		t.Fatalf("exactly one session expected, got %v", sessions)
	}
	// A DIFFERENT requestId creates a NEW session.
	other := do(t, app, http.MethodPost, "/api/uploads/init", map[string]interface{}{
		"filename": "idem.bin", "fileSize": 200000, "environmentId": "env-idem",
		"expiresAt": futureTimestamp(2),
		"requestId": "req-43", "captchaToken": "pass-me", "captchaProvider": "turnstile",
	})
	requireStatus(t, other, http.StatusCreated)
	if decode(t, other)["uploadId"] == a {
		t.Fatalf("different requestId must create a different session")
	}
	// An init WITHOUT requestId also creates a distinct session (no dedup).
	third := do(t, app, http.MethodPost, "/api/uploads/init", map[string]interface{}{
		"filename": "idem.bin", "fileSize": 200000, "environmentId": "env-idem",
		"expiresAt":    futureTimestamp(2),
		"captchaToken": "pass-me", "captchaProvider": "turnstile",
	})
	requireStatus(t, third, http.StatusCreated)
	if sessions, _ := app.Repo.ListUploadSessions(); len(sessions) != 3 {
		t.Fatalf("expected 3 sessions, got %v", sessions)
	}
}

// TestInitExpiredRequestId: an expired session is purged and re-created for
// the same (env, requestId).
func TestInitExpiredRequestId(t *testing.T) {
	app := newTestApp(t, smallChunkSettings())
	first := do(t, app, http.MethodPost, "/api/uploads/init", map[string]interface{}{
		"filename": "old.bin", "fileSize": 100, "environmentId": "env-exp",
		"expiresAt": futureTimestamp(2),
		"requestId": "req-old",
	})
	requireStatus(t, first, http.StatusCreated)
	oldID := decode(t, first)["uploadId"].(string)
	// Force expiry.
	if err := app.Repo.SetUploadExpiryForTest(oldID, time.Now().Add(-2*time.Minute).Unix()); err != nil {
		t.Fatalf("expire: %v", err)
	}
	// Re-init with the same requestId must produce a NEW session (200 status
	// is only for live replays; expired ones create fresh -> 201).
	second := do(t, app, http.MethodPost, "/api/uploads/init", map[string]interface{}{
		"filename": "old.bin", "fileSize": 100, "environmentId": "env-exp",
		"expiresAt": futureTimestamp(2),
		"requestId": "req-old",
	})
	if second.Code != http.StatusCreated {
		t.Fatalf("expired replay should create fresh session (201), got %d (%s)", second.Code, second.Body.String())
	}
	newID := decode(t, second)["uploadId"].(string)
	if newID == oldID {
		t.Fatalf("expired replay must mint a new uploadId")
	}
	// Old row is gone.
	if s, _ := app.Repo.GetUploadSession(oldID); s != nil {
		t.Fatalf("expired session should be purged")
	}
	if sessions, _ := app.Repo.ListUploadSessions(); len(sessions) != 1 || sessions[0].ID != newID {
		t.Fatalf("only the fresh session should remain, got %v", sessions)
	}
}

// TestInitConcurrentSameRequestId: two racing inits with the same key produce
// exactly ONE session (the loser replays the winner's row).
func TestInitConcurrentSameRequestId(t *testing.T) {
	app := newTestApp(t, smallChunkSettings())
	const racers = 8
	ids := make(chan string, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := do(t, app, http.MethodPost, "/api/uploads/init", map[string]interface{}{
				"filename": "race.bin", "fileSize": 4096, "environmentId": "env-race",
				"expiresAt": futureTimestamp(2),
				"requestId": "req-race",
			})
			if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
				t.Errorf("init raced got %d (%s)", rec.Code, rec.Body.String())
				return
			}
			ids <- decode(t, rec)["uploadId"].(string)
		}()
	}
	wg.Wait()
	close(ids)
	unique := map[string]int{}
	for id := range ids {
		unique[id]++
	}
	if len(unique) != 1 {
		t.Fatalf("all racers must map to ONE uploadId, got %v", unique)
	}
	for _, count := range unique {
		if count != racers {
			t.Fatalf("expected %d responses for the shared id, got %d", racers, count)
		}
	}
	if sessions, _ := app.Repo.ListUploadSessions(); len(sessions) != 1 {
		t.Fatalf("exactly one session row expected, got %v", sessions)
	}
}

// TestConfigExposesUploadKnobs ensures the frontend can read server limits.
func TestConfigExposesUploadKnobs(t *testing.T) {
	app := newTestApp(t, smallChunkSettings())
	rec := do(t, app, http.MethodGet, "/api/config", nil)
	requireStatus(t, rec, http.StatusOK)
	payload := decode(t, rec)
	if payload["uploadChunkSizeBytes"] != float64(64<<10) {
		t.Fatalf("chunk size knob missing: %v", payload)
	}
	if _, ok := payload["maxFileSizeBytes"].(float64); !ok {
		t.Fatalf("max size knob missing: %v", payload)
	}
	if _, ok := payload["uploadSessionTTLSeconds"].(float64); !ok {
		t.Fatalf("ttl knob missing: %v", payload)
	}
}
