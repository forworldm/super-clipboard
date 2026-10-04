package repository

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/pixia1234/super-clipboard/backend/internal/config"
)

func TestCalcTotalChunks(t *testing.T) {
	cases := []struct {
		fileSize  int64
		chunkSize int
		want      int
	}{
		{0, 1024, 0},
		{-5, 1024, 0},
		{1, 1024, 1},
		{1024, 1024, 1},
		{1025, 1024, 2},
		{2048, 1024, 2},
		{50 * 1024 * 1024, 1 << 20, 50},
		{100, 0, 0},
	}
	for _, c := range cases {
		if got := CalcTotalChunks(c.fileSize, c.chunkSize); got != c.want {
			t.Fatalf("CalcTotalChunks(%d,%d)=%d want %d", c.fileSize, c.chunkSize, got, c.want)
		}
	}
}

func TestCreateAndGetUploadSession(t *testing.T) {
	repo := newTestRepository(t)
	s, err := repo.CreateUploadSession(CreateUploadSessionParams{
		Filename: "hello.txt", FileSize: 2500, MimeType: "text/plain",
		EnvironmentID: "env-1", ChunkSize: 1024, TTLSeconds: 3600,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if s.ID == "" || s.Status != UploadStatusActive {
		t.Fatalf("unexpected session %+v", s)
	}
	if s.TotalChunks != 3 {
		t.Fatalf("expected 3 chunks, got %d", s.TotalChunks)
	}
	if s.ChunkSize != 1024 {
		t.Fatalf("chunk size should be honoured, got %d", s.ChunkSize)
	}
	got, err := repo.GetUploadSession(s.ID)
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if got.Filename != "hello.txt" || got.FileSize != 2500 || got.EnvironmentID != "env-1" {
		t.Fatalf("fields not persisted: %+v", got)
	}
	if missing, err := repo.GetUploadSession("no-such-id"); err != nil || missing != nil {
		t.Fatalf("unknown id should miss: %v %v", missing, err)
	}
}

func TestCreateUploadSessionPersistsClipParams(t *testing.T) {
	repo := newTestRepository(t)
	code := "abcde"
	token := "tok-1234"
	maxDownloads := 7
	s, err := repo.CreateUploadSession(CreateUploadSessionParams{
		Filename: "clip.bin", FileSize: 100, MimeType: "application/octet-stream",
		EnvironmentID: "env-clip", ChunkSize: 1024, TTLSeconds: 60,
		ClipExpiresAt:    9_999_999_999_999,
		ClipMaxDownloads: &maxDownloads, ClipAccessCode: &code, ClipAccessToken: &token,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := repo.GetUploadSession(s.ID)
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	// Every session carries its owner and its clip params.
	if got.ClipExpiresAt != 9_999_999_999_999 || got.EnvironmentID != "env-clip" {
		t.Fatalf("clip expires/owner not persisted: %+v", got)
	}
	if got.ClipMaxDownloads == nil || *got.ClipMaxDownloads != 7 {
		t.Fatalf("maxDownloads not persisted: %+v", got)
	}
	if got.ClipAccessCode == nil || *got.ClipAccessCode != code {
		t.Fatalf("accessCode not persisted: %+v", got)
	}
	if got.ClipAccessToken == nil || *got.ClipAccessToken != token {
		t.Fatalf("accessToken not persisted: %+v", got)
	}
	// A nameless clip (reachable only through its env owner) persists as such.
	nameless, err := repo.CreateUploadSession(CreateUploadSessionParams{
		Filename: "nameless.bin", FileSize: 10, EnvironmentID: "env-nameless",
		ChunkSize: 1024, TTLSeconds: 60, ClipExpiresAt: 9_999_999_999_999,
	})
	if err != nil {
		t.Fatalf("nameless create: %v", err)
	}
	reloaded, _ := repo.GetUploadSession(nameless.ID)
	if reloaded.EnvironmentID != "env-nameless" || reloaded.ClipAccessCode != nil || reloaded.ClipAccessToken != nil {
		t.Fatalf("nameless session should keep only its owner: %+v", reloaded)
	}
}

// TestCreateUploadSessionRequiresEnvironmentID: upload sessions have no
// ownerless mode, so the repository refuses to persist one.
func TestCreateUploadSessionRequiresEnvironmentID(t *testing.T) {
	repo := newTestRepository(t)
	for _, env := range []string{"", "   "} {
		if _, err := repo.CreateUploadSession(CreateUploadSessionParams{
			Filename: "ownerless.bin", FileSize: 10, EnvironmentID: env,
			ChunkSize: 1024, TTLSeconds: 60, ClipExpiresAt: 9_999_999_999_999,
		}); err == nil {
			t.Fatalf("environment id %q must be refused", env)
		}
	}
	if sessions, _ := repo.ListUploadSessions(); len(sessions) != 0 {
		t.Fatalf("no session row may be written, got %v", sessions)
	}
}

func TestUploadExpectedChunkSize(t *testing.T) {
	repo := newTestRepository(t)
	s, err := repo.CreateUploadSession(CreateUploadSessionParams{
		Filename: "a.bin", FileSize: 2500, ChunkSize: 1024, TTLSeconds: 60,

		EnvironmentID: "env-test"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for i, want := range []int64{1024, 1024, 452} {
		got, ok := s.ExpectedChunkSize(i)
		if !ok || got != want {
			t.Fatalf("chunk %d: got %d,%v want %d", i, got, ok, want)
		}
	}
	if _, ok := s.ExpectedChunkSize(3); ok {
		t.Fatalf("index 3 should be out of range")
	}
	if _, ok := s.ExpectedChunkSize(-1); ok {
		t.Fatalf("negative index should be invalid")
	}
	// Empty file: no chunks.
	empty, err := repo.CreateUploadSession(CreateUploadSessionParams{
		Filename: "empty.txt", FileSize: 0, ChunkSize: 1024, TTLSeconds: 60,

		EnvironmentID: "env-test"})
	if err != nil {
		t.Fatalf("create empty: %v", err)
	}
	if empty.TotalChunks != 0 {
		t.Fatalf("empty file should have 0 chunks, got %d", empty.TotalChunks)
	}
	if _, ok := empty.ExpectedChunkSize(0); ok {
		t.Fatalf("empty file should reject chunk 0")
	}
}

func TestMarkChunkReceivedIdempotent(t *testing.T) {
	repo := newTestRepository(t)
	s, _ := repo.CreateUploadSession(CreateUploadSessionParams{
		Filename: "a.bin", FileSize: 3000, ChunkSize: 1000, TTLSeconds: 60,

		EnvironmentID: "env-test"})
	if err := repo.MarkChunkReceived(s.ID, 1, 1000); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if err := repo.MarkChunkReceived(s.ID, 1, 1000); err != nil {
		t.Fatalf("rewrite should be idempotent: %v", err)
	}
	if err := repo.MarkChunkReceived(s.ID, 0, 1000); err != nil {
		t.Fatalf("mark 0: %v", err)
	}
	received, err := repo.ListReceivedChunks(s.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(received) != 2 || received[0] != 0 || received[1] != 1 {
		t.Fatalf("unexpected received %v", received)
	}
	if err := repo.MarkChunkReceived(s.ID, 5, 100); err == nil {
		t.Fatalf("out-of-range index should fail")
	}
	if err := repo.MarkChunkReceived("missing", 0, 10); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing session should be ErrNoRows, got %v", err)
	}
	if missing := MissingChunks(3, received); len(missing) != 1 || missing[0] != 2 {
		t.Fatalf("missing should be [2], got %v", missing)
	}
}

func TestTryBeginCompleteFlow(t *testing.T) {
	repo := newTestRepository(t)
	s, _ := repo.CreateUploadSession(CreateUploadSessionParams{
		Filename: "a.bin", FileSize: 2000, ChunkSize: 1000, TTLSeconds: 3600,
		StagedPath:    "/tmp/staged",
		EnvironmentID: "env-test"})
	_ = repo.MarkChunkReceived(s.ID, 0, 1000)
	if _, _, err := repo.TryBeginComplete(s.ID); err == nil {
		t.Fatalf("should fail with missing chunks")
	} else {
		var missing *MissingChunksError
		if !errors.As(err, &missing) {
			t.Fatalf("expected MissingChunksError, got %T %v", err, err)
		}
		if len(missing.Missing) != 1 || missing.Missing[0] != 1 {
			t.Fatalf("missing should be [1], got %v", missing.Missing)
		}
	}
	// Session must stay active for retry.
	if cur, _ := repo.GetUploadSession(s.ID); cur.Status != UploadStatusActive {
		t.Fatalf("status should stay active, got %s", cur.Status)
	}
	_ = repo.MarkChunkReceived(s.ID, 1, 1000)
	completing, received, err := repo.TryBeginComplete(s.ID)
	if err != nil {
		t.Fatalf("begin complete: %v", err)
	}
	if completing.Status != UploadStatusCompleting || completing.StagedPath != s.StagedPath {
		t.Fatalf("unexpected %+v", completing)
	}
	if len(received) != 2 {
		t.Fatalf("expected 2 received, got %v", received)
	}
	// Second concurrent completer must lose.
	if _, _, err := repo.TryBeginComplete(s.ID); err == nil {
		t.Fatalf("second begin should fail")
	} else {
		var c *SessionCompletingError
		if !errors.As(err, &c) {
			t.Fatalf("expected CompletingError, got %T", err)
		}
	}
	// Rollback then retry: the pre-allocated file + receipts survive, because the
	// bytes are already in their final place and only the clip insert failed.
	staged, err := repo.FailComplete(s.ID)
	if err != nil || staged != s.StagedPath {
		t.Fatalf("fail complete: %v %q", err, staged)
	}
	if cur, _ := repo.GetUploadSession(s.ID); cur.Status != UploadStatusActive || cur.StagedPath != s.StagedPath {
		t.Fatalf("should roll back to active keeping its storage path, got %+v", cur)
	}
	if got, _ := repo.ListReceivedChunks(s.ID); len(got) != 2 {
		t.Fatalf("receipts must survive the rollback, got %v", got)
	}
	if _, _, err := repo.TryBeginComplete(s.ID); err != nil {
		t.Fatalf("retry begin: %v", err)
	}
	completed, err := repo.CompleteUploadSession(s.ID, "clip-1")
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if completed.Status != UploadStatusCompleted || completed.ClipID != "clip-1" {
		t.Fatalf("unexpected %+v", completed)
	}
	if _, _, err := repo.TryBeginComplete(s.ID); err == nil {
		t.Fatalf("completed session should reject begin")
	} else {
		var c *SessionCompletedError
		if !errors.As(err, &c) {
			t.Fatalf("expected CompletedError, got %T", err)
		}
	}
}

func TestAbortUploadSession(t *testing.T) {
	repo := newTestRepository(t)
	s, _ := repo.CreateUploadSession(CreateUploadSessionParams{
		Filename: "a.bin", FileSize: 100, ChunkSize: 1000, TTLSeconds: 60,

		EnvironmentID: "env-test"})
	_ = repo.MarkChunkReceived(s.ID, 0, 100)
	aborted, err := repo.AbortUploadSession(s.ID)
	if err != nil {
		t.Fatalf("abort: %v", err)
	}
	if aborted.ID != s.ID {
		t.Fatalf("wrong session %+v", aborted)
	}
	if cur, _ := repo.GetUploadSession(s.ID); cur != nil {
		t.Fatalf("row should be gone")
	}
	if _, err := repo.AbortUploadSession(s.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second abort should be ErrNoRows, got %v", err)
	}
	// Completed with clip is not abortable.
	s2, _ := repo.CreateUploadSession(CreateUploadSessionParams{
		Filename: "b.bin", FileSize: 0, ChunkSize: 1000, TTLSeconds: 60,

		EnvironmentID: "env-test"})
	if _, _, err := repo.TryBeginComplete(s2.ID); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := repo.CompleteUploadSession(s2.ID, "clip-9"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, err := repo.AbortUploadSession(s2.ID); err == nil {
		t.Fatalf("completed-with-clip should not abort")
	}
	// Completed file-only IS abortable (drops staged file).
	s3, _ := repo.CreateUploadSession(CreateUploadSessionParams{
		Filename: "c.bin", FileSize: 0, ChunkSize: 1000, TTLSeconds: 60,

		EnvironmentID: "env-test"})
	if _, _, err := repo.TryBeginComplete(s3.ID); err != nil {
		t.Fatalf("begin s3: %v", err)
	}
	if _, err := repo.CompleteUploadSession(s3.ID, ""); err != nil {
		t.Fatalf("complete s3: %v", err)
	}
	if _, err := repo.AbortUploadSession(s3.ID); err != nil {
		t.Fatalf("file-only completed should abort: %v", err)
	}
}

func TestPurgeExpiredUploads(t *testing.T) {
	repo := newTestRepository(t)
	alive, _ := repo.CreateUploadSession(CreateUploadSessionParams{
		Filename: "alive.bin", FileSize: 10, ChunkSize: 1000, TTLSeconds: 3600,

		EnvironmentID: "env-test"})
	stale, _ := repo.CreateUploadSession(CreateUploadSessionParams{
		Filename: "stale.bin", FileSize: 10, ChunkSize: 1000, TTLSeconds: 3600,

		EnvironmentID: "env-test"})
	_ = repo.MarkChunkReceived(stale.ID, 0, 10)
	// Force expiry.
	if _, err := repo.db.Exec("UPDATE upload_sessions SET expires_at = ? WHERE id = ?",
		time.Now().Add(-time.Minute).Unix(), stale.ID); err != nil {
		t.Fatalf("expire: %v", err)
	}
	victims, err := repo.PurgeExpiredUploads(time.Now().Unix())
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if len(victims) != 1 || victims[0].UploadID != stale.ID {
		t.Fatalf("unexpected victims %v", victims)
	}
	if cur, _ := repo.GetUploadSession(stale.ID); cur != nil {
		t.Fatalf("stale should be gone")
	}
	if cur, _ := repo.GetUploadSession(alive.ID); cur == nil {
		t.Fatalf("alive should survive")
	}
	if chunks, _ := repo.ListReceivedChunks(stale.ID); len(chunks) != 0 {
		t.Fatalf("stale chunks should be gone, got %v", chunks)
	}
}

func TestResetStuckCompleting(t *testing.T) {
	repo := newTestRepository(t)
	s, _ := repo.CreateUploadSession(CreateUploadSessionParams{
		Filename: "a.bin", FileSize: 0, ChunkSize: 1000, TTLSeconds: 3600,
		StagedPath:    "/tmp/stuck",
		EnvironmentID: "env-test"})
	if _, _, err := repo.TryBeginComplete(s.ID); err != nil {
		t.Fatalf("begin: %v", err)
	}
	stuck, err := repo.ResetStuckCompleting()
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if len(stuck) != 1 || stuck[0].UploadID != s.ID || stuck[0].StagedPath != "/tmp/stuck" {
		t.Fatalf("unexpected stuck %v", stuck)
	}
	if cur, _ := repo.GetUploadSession(s.ID); cur.Status != UploadStatusActive || cur.StagedPath != "/tmp/stuck" {
		t.Fatalf("should reset to active keeping its pre-allocated file, got %+v", cur)
	}
	if again, _ := repo.ResetStuckCompleting(); len(again) != 0 {
		t.Fatalf("second reset should be empty, got %v", again)
	}
}

// TestCreateOrGetUploadSession covers the (env, requestID) idempotency key.
func TestCreateOrGetUploadSession(t *testing.T) {
	repo := newTestRepository(t)
	params := CreateUploadSessionParams{
		Filename: "idem.bin", FileSize: 2048, ChunkSize: 1024, TTLSeconds: 3600,
		EnvironmentID: "env-idem", RequestID: "req-1",
	}
	s1, created1, err := repo.CreateOrGetUploadSession(params)
	if err != nil || !created1 {
		t.Fatalf("first init must create: created=%v err=%v", created1, err)
	}
	_ = repo.MarkChunkReceived(s1.ID, 0, 1024)
	// Replay: same key returns the SAME session with progress intact.
	s2, created2, err := repo.CreateOrGetUploadSession(params)
	if err != nil || created2 {
		t.Fatalf("replay must not create a new session: created=%v err=%v", created2, err)
	}
	if s2.ID != s1.ID {
		t.Fatalf("replay returned a different session: %s vs %s", s2.ID, s1.ID)
	}
	if got, _ := repo.ListReceivedChunks(s1.ID); len(got) != 1 {
		t.Fatalf("progress must survive replay, got %v", got)
	}
	// Different requestId -> new session.
	params.RequestID = "req-2"
	s3, created3, err := repo.CreateOrGetUploadSession(params)
	if err != nil || !created3 || s3.ID == s1.ID {
		t.Fatalf("different requestId must create: %+v created=%v err=%v", s3, created3, err)
	}
	// Expired key: rolled out and replaced by a fresh session.
	if err := repo.SetUploadExpiryForTest(s1.ID, time.Now().Add(-time.Hour).Unix()); err != nil {
		t.Fatalf("expire: %v", err)
	}
	params.RequestID = "req-1"
	s4, created4, err := repo.CreateOrGetUploadSession(params)
	if err != nil || !created4 {
		t.Fatalf("expired key must create fresh: created=%v err=%v", created4, err)
	}
	if s4.ID == s1.ID {
		t.Fatalf("expired key must mint a new upload id")
	}
	if old, _ := repo.GetUploadSession(s1.ID); old != nil {
		t.Fatalf("expired row must be purged")
	}
	// Without requestId every call creates.
	params.RequestID = ""
	_, cA, _ := repo.CreateOrGetUploadSession(params)
	_, cB, _ := repo.CreateOrGetUploadSession(params)
	if !cA || !cB {
		t.Fatalf("requestId-less inits always create: %v %v", cA, cB)
	}
}

func TestClearChunkReceiptsAndDeleteOne(t *testing.T) {
	repo := newTestRepository(t)
	s, _ := repo.CreateUploadSession(CreateUploadSessionParams{
		Filename: "a.bin", FileSize: 3000, ChunkSize: 1000, TTLSeconds: 60,

		EnvironmentID: "env-test"})
	_ = repo.MarkChunkReceived(s.ID, 0, 1000)
	_ = repo.MarkChunkReceived(s.ID, 1, 1000)
	// Dropping a single receipt makes exactly that range "missing" again (used
	// when a retried PUT left a half-overwritten range behind).
	if err := repo.DeleteChunkReceipt(s.ID, 1); err != nil {
		t.Fatalf("delete receipt: %v", err)
	}
	if got, _ := repo.ListReceivedChunks(s.ID); len(got) != 1 || got[0] != 0 {
		t.Fatalf("unexpected %v", got)
	}
	if err := repo.MarkChunkReceived(s.ID, 1, 1000); err != nil {
		t.Fatalf("re-mark: %v", err)
	}
	// Clearing every receipt is the recovery path for a lost byte container.
	if err := repo.ClearChunkReceipts(s.ID); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got, _ := repo.ListReceivedChunks(s.ID); len(got) != 0 {
		t.Fatalf("all receipts should be gone, got %v", got)
	}
	exists, err := repo.UploadSessionExists(s.ID)
	if err != nil || !exists {
		t.Fatalf("should exist: %v %v", exists, err)
	}
	if exists, _ := repo.UploadSessionExists("nope"); exists {
		t.Fatalf("should not exist")
	}
	if sessions, err := repo.ListUploadSessions(); err != nil || len(sessions) != 1 {
		t.Fatalf("list: %v %v", sessions, err)
	}
}

// TestFailCompleteKeepsQuotaReservation locks the ledger invariant: a rolled
// back complete leaves the session `active` WITH its chunks on disk, so it must
// keep its reservation. Releasing it there would let bytes live on disk with no
// accounting (quota bypass) and would contradict RecomputeUploadQuota, which
// sums file_size over active sessions that still hold a reservation.
func TestFailCompleteKeepsQuotaReservation(t *testing.T) {
	repo := newTestRepository(t)
	s, err := repo.CreateUploadSession(CreateUploadSessionParams{
		Filename: "rollback.bin", FileSize: 700, ChunkSize: 1000, TTLSeconds: 3600,
		EnvironmentID: "env-quota", ClipExpiresAt: 9_999_999_999_999,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.ReserveUploadQuota(700, 0); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := repo.MarkChunkReceived(s.ID, 0, 700); err != nil {
		t.Fatalf("mark chunk: %v", err)
	}
	if _, _, err := repo.TryBeginComplete(s.ID); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := repo.FailComplete(s.ID); err != nil {
		t.Fatalf("fail complete: %v", err)
	}
	if got, _ := repo.UploadQuotaValue(); got != 700 {
		t.Fatalf("reserved = %d after rollback, want the 700 bytes kept", got)
	}
	if recomputed, _, err := repo.RecomputeUploadQuota(); err != nil || recomputed != 700 {
		t.Fatalf("recompute = %d (err %v), want 700: ledger must agree with live sessions", recomputed, err)
	}
	// The terminal transition (cancel) still returns the reservation exactly once.
	if _, err := repo.AbortUploadSession(s.ID); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if got, _ := repo.UploadQuotaValue(); got != 0 {
		t.Fatalf("reserved = %d after abort, want 0", got)
	}
}

// TestPurgeExpiredUploadsSkipsCompleting: a session mid-merge must never be torn
// down by the expiry worker -- its staged file is being written right now and
// unlinking it would leave the clip inserted moments later pointing at nothing.
func TestPurgeExpiredUploadsSkipsCompleting(t *testing.T) {
	repo := newTestRepository(t)
	s, _ := repo.CreateUploadSession(CreateUploadSessionParams{
		Filename: "inflight.bin", FileSize: 10, ChunkSize: 1000, TTLSeconds: 3600,
		EnvironmentID: "env-inflight", ClipExpiresAt: 9_999_999_999_999,
	})
	if err := repo.MarkChunkReceived(s.ID, 0, 10); err != nil {
		t.Fatalf("mark chunk: %v", err)
	}
	if _, _, err := repo.TryBeginComplete(s.ID); err != nil {
		t.Fatalf("begin: %v", err)
	}
	// TTL elapses while the merge is still running.
	if _, err := repo.db.Exec("UPDATE upload_sessions SET expires_at = ? WHERE id = ?",
		time.Now().Add(-time.Minute).Unix(), s.ID); err != nil {
		t.Fatalf("expire: %v", err)
	}
	victims, err := repo.PurgeExpiredUploads(time.Now().Unix())
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if len(victims) != 0 {
		t.Fatalf("an in-flight merge must not be purged, got %v", victims)
	}
	if cur, _ := repo.GetUploadSession(s.ID); cur == nil || cur.Status != UploadStatusCompleting {
		t.Fatalf("completing session must survive the purge: %+v", cur)
	}
	// Once the merge rolls back to active, the same row becomes purgeable.
	if _, err := repo.FailComplete(s.ID); err != nil {
		t.Fatalf("fail complete: %v", err)
	}
	victims, err = repo.PurgeExpiredUploads(time.Now().Unix())
	if err != nil || len(victims) != 1 || victims[0].UploadID != s.ID {
		t.Fatalf("rolled-back expired session should be purged, got %v (err %v)", victims, err)
	}
}

// TestInitReplayRefusesStaleInFlightSession: when the (env, requestId) slot is
// held by a session that already expired while still mid-merge, init must NOT
// answer 200 with that unusable id -- it reports a retryable conflict instead.
func TestInitReplayRefusesStaleInFlightSession(t *testing.T) {
	repo := newTestRepository(t)
	params := CreateUploadSessionParams{
		Filename: "stale.bin", FileSize: 100, ChunkSize: 1000, TTLSeconds: 60,
		EnvironmentID: "env-stale", RequestID: "req-stale",
		ClipExpiresAt: 9_999_999_999_999,
	}
	s, created, err := repo.CreateOrGetUploadSession(params)
	if err != nil || !created {
		t.Fatalf("first init should create: %v %v", created, err)
	}
	if err := repo.MarkChunkReceived(s.ID, 0, 100); err != nil {
		t.Fatalf("mark chunk: %v", err)
	}
	if _, _, err := repo.TryBeginComplete(s.ID); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := repo.db.Exec("UPDATE upload_sessions SET expires_at = ? WHERE id = ?",
		time.Now().Add(-time.Minute).Unix(), s.ID); err != nil {
		t.Fatalf("expire: %v", err)
	}
	replayed, created, err := repo.CreateOrGetUploadSession(params)
	var unavailable *SessionUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("expected SessionUnavailableError, got replayed=%+v created=%v err=%v", replayed, created, err)
	}
	if unavailable.Session == nil || unavailable.Session.ID != s.ID {
		t.Fatalf("error should carry the blocking session, got %+v", unavailable.Session)
	}
	// A different requestId is unaffected (new session, no conflict).
	other := params
	other.RequestID = "req-other"
	if _, created, err := repo.CreateOrGetUploadSession(other); err != nil || !created {
		t.Fatalf("unrelated key should still create: %v %v", created, err)
	}
}

// TestCompleteShortensSessionExpiry: a finished session drops the long resume
// TTL and keeps only the short idempotency window, so its row (and the
// (environment_id, request_id) slot it holds) disappears quickly instead of
// pinning the row for the whole resume window.
func TestCompleteShortensSessionExpiry(t *testing.T) {
	const completedTTL = 5
	repo := newTestRepositoryWith(t, func(s *config.Settings) { s.CompletedUploadTTLSeconds = completedTTL })
	session := createReservingSession(t, repo, "short.bin", 2048, 0)
	now := time.Now().UTC().Unix()
	if session.ExpiresAt <= now+60 {
		t.Fatalf("a live session must keep the long resume window, got %d", session.ExpiresAt)
	}
	for i := 0; i < session.TotalChunks; i++ {
		if err := repo.MarkChunkReceived(session.ID, i, 1024); err != nil {
			t.Fatalf("mark chunk %d: %v", i, err)
		}
	}
	if _, _, err := repo.TryBeginComplete(session.ID); err != nil {
		t.Fatalf("begin complete: %v", err)
	}
	completed, err := repo.CompleteUploadSession(session.ID, "clip-short")
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if completed.ExpiresAt > time.Now().UTC().Unix()+completedTTL {
		t.Fatalf("completed session expires_at = %d, want <= now+%d", completed.ExpiresAt, completedTTL)
	}
	if completed.ExpiresAt >= session.ExpiresAt {
		t.Fatalf("complete must shorten the expiry (%d -> %d)", session.ExpiresAt, completed.ExpiresAt)
	}
	// The shortened expiry is what lets the worker drop the row: past it the
	// session is purged, and the clip it produced is reported so the file lives on.
	victims, err := repo.PurgeExpiredUploads(time.Now().UTC().Unix() + completedTTL + 1)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if len(victims) != 1 || !victims[0].HasClip {
		t.Fatalf("expected one clip-backed victim, got %+v", victims)
	}
	if purged, _ := repo.GetUploadSession(completed.ID); purged != nil {
		t.Fatalf("the finished session row must be gone, got %+v", purged)
	}
}

// TestAcceptsReplayIgnoresExpiryForCompletedSession locks the idempotency
// contract: expires_at is a CLEANUP knob for a finished session, never a reason
// to answer a retry with "gone" (which would make the client upload again).
func TestAcceptsReplayIgnoresExpiryForCompletedSession(t *testing.T) {
	now := time.Now().UTC().Unix()
	active := &UploadSession{Status: UploadStatusActive, ExpiresAt: now + 1}
	expired := &UploadSession{Status: UploadStatusActive, ExpiresAt: now - 1}
	completed := &UploadSession{Status: UploadStatusCompleted, ExpiresAt: now - 1}
	completing := &UploadSession{Status: UploadStatusCompleting, ExpiresAt: now - 1}

	if !active.AcceptsReplay(now) {
		t.Fatal("a live session must accept a replay")
	}
	if expired.AcceptsReplay(now) {
		t.Fatal("an expired live session must not accept a replay")
	}
	if !completed.AcceptsReplay(now) {
		t.Fatal("a completed session must stay replayable whatever expires_at says")
	}
	if completing.AcceptsReplay(now) {
		t.Fatal("an expired completing session must not accept a replay")
	}
	// A concurrent init must replay (never replace) a completed key too.
	repo := newTestRepository(t)
	params := CreateUploadSessionParams{
		Filename: "replay.bin", FileSize: 64, MimeType: "application/octet-stream",
		EnvironmentID: "env-replay", ChunkSize: 1024, TTLSeconds: 3600, RequestID: "req-done",
	}
	session, created, err := repo.CreateOrGetUploadSession(params)
	if err != nil || !created {
		t.Fatalf("first init: created=%v err=%v", created, err)
	}
	if err := repo.MarkChunkReceived(session.ID, 0, 64); err != nil {
		t.Fatalf("mark chunk: %v", err)
	}
	if _, _, err := repo.TryBeginComplete(session.ID); err != nil {
		t.Fatalf("begin complete: %v", err)
	}
	if _, err := repo.CompleteUploadSession(session.ID, "clip-replay"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	// Expiry already elapsed (the replay window is short) but the row still exists.
	if err := repo.SetUploadExpiryForTest(session.ID, 1); err != nil {
		t.Fatalf("force expiry: %v", err)
	}
	replayed, created, err := repo.CreateOrGetUploadSession(params)
	if err != nil {
		t.Fatalf("replay init: %v", err)
	}
	if created || replayed == nil || replayed.ID != session.ID {
		t.Fatalf("a finished key must be replayed as-is, got created=%v session=%+v", created, replayed)
	}
	if sessions, _ := repo.ListUploadSessions(); len(sessions) != 1 {
		t.Fatalf("a replay must not open a second session, got %d", len(sessions))
	}
}
