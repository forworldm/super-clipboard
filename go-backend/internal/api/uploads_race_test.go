package api

// Race-condition probes for the chunked upload protocol. They complement the
// deterministic tests in uploads_test.go: every round lets the scheduler pick a
// different interleaving and asserts the OUTCOME invariants (never a 5xx, never
// a corrupt clip, never a lost reservation).

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestInitReplayStaleInFlightIsConflict: the idempotency slot is held by a
// session that expired while still `completing`. Init must answer with a
// retryable conflict -- never 200 + an uploadId whose /complete can only 409.
func TestInitReplayStaleInFlightIsConflict(t *testing.T) {
	app := newTestApp(t, smallChunkSettings())
	body := map[string]interface{}{
		"filename": "stale.bin", "fileSize": 1024, "mimeType": "application/octet-stream",
		"environmentId": "env-stale", "requestId": "req-stale",
		"expiresAt": futureTimestamp(2),
	}
	init := initUploadBody(t, app, body)
	uploadID := init["uploadId"].(string)
	if err := app.Repo.MarkChunkReceived(uploadID, 0, 1024); err != nil {
		t.Fatalf("mark chunk: %v", err)
	}
	if _, _, err := app.Repo.TryBeginComplete(uploadID); err != nil {
		t.Fatalf("begin complete: %v", err)
	}
	if err := app.Repo.SetUploadExpiryForTest(uploadID, time.Now().Add(-time.Minute).Unix()); err != nil {
		t.Fatalf("expire: %v", err)
	}

	rec := do(t, app, http.MethodPost, "/api/uploads/init", body)
	requireStatus(t, rec, http.StatusConflict)
	if _, ok := decode(t, rec)["detail"]; !ok {
		t.Fatalf("conflict body must carry a detail, got %s", rec.Body.String())
	}
	// The blocking session is still there (its merge was not torn down).
	if cur, _ := app.Repo.GetUploadSession(uploadID); cur == nil || cur.Status != "completing" {
		t.Fatalf("in-flight session must survive, got %+v", cur)
	}
	// Its reservation is still held, so no quota was leaked by the refusal.
	if reserved, _ := app.Repo.UploadQuotaValue(); reserved != 1024 {
		t.Fatalf("reserved = %d, want 1024 (still held by the in-flight session)", reserved)
	}
}

// TestQuotaHeldAfterCompleteRollback proves the ledger invariant end-to-end: a
// 409 rollback (access code taken by a concurrent complete) leaves the session
// active WITH its chunks on disk, so its reservation must still be held; the
// cancel path then returns it exactly once.
func TestQuotaHeldAfterCompleteRollback(t *testing.T) {
	app := newTestApp(t, smallChunkSettings())
	env := "env-quota-rollback"
	data := deterministicBytes(4096)

	init1 := initClipUpload(t, app, "q1.bin", int64(len(data)), "application/octet-stream", env,
		map[string]interface{}{"accessCode": "13579"})
	init2 := initClipUpload(t, app, "q2.bin", int64(len(data)), "application/octet-stream", env,
		map[string]interface{}{"accessCode": "13579"})
	id1, id2 := init1["uploadId"].(string), init2["uploadId"].(string)
	requireStatus(t, putChunk(t, app, id1, 0, data), http.StatusOK)
	requireStatus(t, putChunk(t, app, id2, 0, data), http.StatusOK)

	reservedBefore, _ := app.Repo.UploadQuotaValue()
	requireStatus(t, completeUpload(t, app, id1, nil), http.StatusCreated)
	// The losing complete rolls back; only the winner's bytes stop being reserved.
	requireStatus(t, completeUpload(t, app, id2, nil), http.StatusConflict)

	reservedAfter, _ := app.Repo.UploadQuotaValue()
	wantAfter := reservedBefore - int64(len(data))
	if reservedAfter != wantAfter {
		t.Fatalf("reserved = %d after rollback, want %d (rolled-back session keeps its reservation)",
			reservedAfter, wantAfter)
	}
	if recomputed, _, err := app.Repo.RecomputeUploadQuota(); err != nil || recomputed != wantAfter {
		t.Fatalf("recompute = %d (err %v), want %d: ledger must match live sessions", recomputed, err, wantAfter)
	}
	// Cancelling the rolled-back session returns its reservation exactly once.
	requireStatus(t, do(t, app, http.MethodDelete, "/api/uploads/"+id2, nil), http.StatusOK)
	if reserved, _ := app.Repo.UploadQuotaValue(); reserved != 0 {
		t.Fatalf("reserved = %d after cancel, want 0", reserved)
	}
}

// TestConcurrentChunkVsCompleteIsRaceFree races the LAST chunk PUT against
// /complete over several rounds. Legal outcomes per round:
//
//	a) PUT wins                   -> complete 201, clip content == original bytes
//	b) complete still sees a gap  -> 400 with a `missing` list, retry succeeds
//	c) another completer won      -> 409/200 replay of the SAME clip
//
// Forbidden: any 5xx, a clip with truncated/mismatched content, or two clips.
func TestConcurrentChunkVsCompleteIsRaceFree(t *testing.T) {
	app := newTestApp(t, smallChunkSettings())
	chunkSize := 64 << 10
	totalChunks := 3
	fileSize := chunkSize*2 + 1234
	data := deterministicBytes(fileSize)

	for round := 0; round < 6; round++ {
		env := fmt.Sprintf("env-put-vs-complete-%d", round)
		init := initUpload(t, app, fmt.Sprintf("pvc-%d.bin", round), int64(fileSize), "application/octet-stream", env)
		uploadID := init["uploadId"].(string)
		for i := 0; i < totalChunks-1; i++ {
			requireStatus(t, putChunk(t, app, uploadID, i, sliceFor(chunkSize, data, i)), http.StatusOK)
		}

		barrier := make(chan struct{})
		var wg sync.WaitGroup
		putResult := make(chan *httptest.ResponseRecorder, 1)
		completeResult := make(chan *httptest.ResponseRecorder, 1)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-barrier
			putResult <- putChunk(t, app, uploadID, totalChunks-1, sliceFor(chunkSize, data, totalChunks-1))
		}()
		go func() {
			defer wg.Done()
			<-barrier
			completeResult <- completeUpload(t, app, uploadID, nil)
		}()
		close(barrier)
		wg.Wait()
		put, complete := <-putResult, <-completeResult

		// The racing PUT may legitimately lose the race to complete/cleanup.
		switch put.Code {
		case http.StatusOK, http.StatusConflict, http.StatusNotFound, http.StatusGone:
		default:
			t.Fatalf("round %d: racing PUT returned %d (%s)", round, put.Code, put.Body.String())
		}
		if put.Code >= http.StatusInternalServerError {
			t.Fatalf("round %d: racing PUT must never 5xx", round)
		}

		var clipID string
		switch complete.Code {
		case http.StatusCreated:
			clipID = decode(t, complete)["id"].(string)
		case http.StatusBadRequest:
			// Gap seen because the receipt was not committed yet: the missing
			// list must be exactly the racing chunk, and a retry must succeed.
			raw := decode(t, complete)
			missing, _ := raw["missing"].([]interface{})
			if len(missing) != 1 || missing[0] != float64(totalChunks-1) {
				t.Fatalf("round %d: unexpected missing list %v", round, raw)
			}
			requireStatus(t, putChunk(t, app, uploadID, totalChunks-1, sliceFor(chunkSize, data, totalChunks-1)), http.StatusOK)
			retry := completeUpload(t, app, uploadID, nil)
			requireStatus(t, retry, http.StatusCreated)
			clipID = decode(t, retry)["id"].(string)
		case http.StatusConflict:
			// The merge is already running elsewhere: it can only be replayed.
			replay := completeUpload(t, app, uploadID, nil)
			requireStatus(t, replay, http.StatusOK)
			clipID = decode(t, replay)["id"].(string)
		default:
			t.Fatalf("round %d: complete returned %d (%s)", round, complete.Code, complete.Body.String())
		}

		// Invariant 1: exactly one clip for the environment, byte-identical.
		listed := decode(t, do(t, app, http.MethodGet, "/api/clips?environmentId="+env, nil))
		items := listed["items"].([]interface{})
		if len(items) != 1 {
			t.Fatalf("round %d: exactly one clip expected, got %v", round, listed)
		}
		dl := do(t, app, http.MethodGet, "/api/clips/"+clipID+"/file?environmentId="+env, nil)
		requireStatus(t, dl, http.StatusOK)
		if !bytes.Equal(dl.Body.Bytes(), data) {
			t.Fatalf("round %d: assembled clip is corrupt (got %d bytes, want %d)", round, dl.Body.Len(), len(data))
		}
		// Invariant 2: the session is committed and linked to that clip.
		session, _ := app.Repo.GetUploadSession(uploadID)
		if session == nil || session.Status != "completed" || session.ClipID != clipID {
			t.Fatalf("round %d: session state %+v", round, session)
		}
		// Invariant 3: the chunk dir is gone (no leftovers after the merge).
		if _, err := app.Repo.ListReceivedChunks(uploadID); err != nil {
			t.Fatalf("round %d: chunk receipts unreadable: %v", round, err)
		}
	}
}
