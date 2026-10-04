package repository

// Coverage for the bounded retries of CreateOrGetUploadSession.
//
// The retry loop exists for one race: two inits share an idempotency key and the
// loser's INSERT is refused by the partial UNIQUE index on
// (environment_id, request_id). That race is inherently timing dependent, so
// these tests reproduce its OBSERVABLE preconditions with a SQLite trigger
// instead of threads:
//
//   - the trigger aborts the first INSERT of the key with the very same
//     "UNIQUE constraint failed" message a racing insert would produce, while
//     (optionally) publishing the winner row;
//   - RAISE(FAIL) keeps the trigger's own bookkeeping (a counter, the winner
//     row) although the statement is aborted, which is exactly the "winner may
//     appear or vanish between the failed insert and the retry" situation.
//
// The tests therefore pin, deterministically, that the function
//   1) retries and publishes its own session when the key is free again,
//   2) retries and replays the winner when one appeared in the meantime,
//   3) stays bounded (maxInitAttempts inserts) when every attempt conflicts.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// uniqueViolationMessage mirrors what SQLite (modernc and cgo drivers alike)
// reports for a UNIQUE constraint failure on the idempotency index.
const uniqueViolationMessage = "UNIQUE constraint failed: upload_sessions.request_id"

// seedInitProbe creates the side tables a probe trigger needs:
//
//	probe_attempts(n)  -- one row per INSERT that the trigger aborted
//	probe_flags(fired) -- single row, the "first attempt" latch
func seedInitProbe(t *testing.T, repo *ClipRepository) {
	t.Helper()
	statements := []string{
		`CREATE TABLE IF NOT EXISTS probe_attempts (n INTEGER NOT NULL, at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS probe_flags (fired INTEGER NOT NULL)`,
		`INSERT INTO probe_flags (fired) SELECT 0 WHERE NOT EXISTS (SELECT 1 FROM probe_flags)`,
	}
	for _, statement := range statements {
		if _, err := repo.db.Exec(statement); err != nil {
			t.Fatalf("init probe schema: %v", err)
		}
	}
}

func probeAttemptCount(t *testing.T, repo *ClipRepository) int {
	t.Helper()
	var count int
	if err := repo.db.QueryRow("SELECT COUNT(*) FROM probe_attempts").Scan(&count); err != nil {
		t.Fatalf("count probe attempts: %v", err)
	}
	return count
}

func sessionRowsWithKey(t *testing.T, repo *ClipRepository, env string, requestID string) int {
	t.Helper()
	var count int
	if err := repo.db.QueryRow(
		"SELECT COUNT(*) FROM upload_sessions WHERE environment_id = ? AND request_id = ?",
		env, requestID).Scan(&count); err != nil {
		t.Fatalf("count sessions for key: %v", err)
	}
	return count
}

// TestCreateOrGetUploadSessionRetriesTransientUniqueConflict proves the loop body
// really runs again: the first INSERT is refused with a UNIQUE violation while no
// row carries the key (the winner was purged before we could read it), and the
// retry must publish the session instead of failing the whole init.
func TestCreateOrGetUploadSessionRetriesTransientUniqueConflict(t *testing.T) {
	repo := newTestRepository(t)
	seedInitProbe(t, repo)
	// Abort the first INSERT of this key, recording that it happened.
	trigger := fmt.Sprintf(`CREATE TRIGGER probe_first_insert BEFORE INSERT ON upload_sessions
		WHEN NEW.request_id = 'req-transient' AND (SELECT fired FROM probe_flags) = 0
		BEGIN
			UPDATE probe_flags SET fired = 1;
			INSERT INTO probe_attempts (n, at) VALUES (1, NEW.created_at);
			SELECT RAISE(FAIL, '%s');
		END`, uniqueViolationMessage)
	if _, err := repo.db.Exec(trigger); err != nil {
		t.Fatalf("create probe trigger: %v", err)
	}

	params := CreateUploadSessionParams{
		Filename: "transient.bin", FileSize: 4096, ChunkSize: 1024, TTLSeconds: 3600,
		EnvironmentID: "env-transient", RequestID: "req-transient",
		ClipExpiresAt: time.Now().Add(2 * time.Hour).UnixMilli(),
	}
	session, created, err := repo.CreateOrGetUploadSession(params)
	if err != nil {
		t.Fatalf("a transient UNIQUE conflict must be retried, got %v", err)
	}
	if !created || session == nil {
		t.Fatalf("the retry must create the session: created=%v session=%+v", created, session)
	}
	if got := probeAttemptCount(t, repo); got != 1 {
		t.Fatalf("expected exactly one aborted insert attempt, got %d", got)
	}
	if got := sessionRowsWithKey(t, repo, "env-transient", "req-transient"); got != 1 {
		t.Fatalf("expected exactly one session for the key, got %d", got)
	}
	// The published row is the one we were handed, fully usable.
	stored, err := repo.GetUploadSession(session.ID)
	if err != nil || stored == nil {
		t.Fatalf("retried session must be persisted: %+v %v", stored, err)
	}
	if stored.RequestID != "req-transient" || stored.EnvironmentID != "env-transient" {
		t.Fatalf("retried session lost its idempotency key: %+v", stored)
	}
	// Replaying the same key now returns the very same session without inserting.
	replayed, createdAgain, err := repo.CreateOrGetUploadSession(params)
	if err != nil || createdAgain || replayed.ID != session.ID {
		t.Fatalf("replay after the retry: created=%v id=%v err=%v", createdAgain, replayed, err)
	}
	if got := probeAttemptCount(t, repo); got != 1 {
		t.Fatalf("the replay must not have inserted again, attempts = %d", got)
	}
}

// TestCreateOrGetUploadSessionRetryReplaysWinner: the first INSERT is refused
// because a competing init published the winner row at the same moment, and the
// retry must return THAT session (created=false) instead of inserting a second
// one or failing.
func TestCreateOrGetUploadSessionRetryReplaysWinner(t *testing.T) {
	repo := newTestRepository(t)
	seedInitProbe(t, repo)
	winnerID := "11111111-2222-3333-4444-555555555555"
	now := time.Now().UTC().Unix()
	expires := time.Now().Add(time.Hour).Unix()
	trigger := fmt.Sprintf(`CREATE TRIGGER probe_race_winner BEFORE INSERT ON upload_sessions
		WHEN NEW.request_id = 'req-winner' AND (SELECT fired FROM probe_flags) = 0
		BEGIN
			UPDATE probe_flags SET fired = 1;
			INSERT INTO probe_attempts (n, at) VALUES (2, NEW.created_at);
			INSERT INTO upload_sessions (
				id, filename, file_size, mime_type, chunk_size, total_chunks,
				status, environment_id, created_at, updated_at, expires_at, request_id
			) VALUES (
				'%s', 'winner.bin', 2048, 'application/octet-stream', 1024, 2,
				'active', 'env-winner', %d, %d, %d, 'req-winner'
			);
			SELECT RAISE(FAIL, '%s');
		END`, winnerID, now, now, expires, uniqueViolationMessage)
	if _, err := repo.db.Exec(trigger); err != nil {
		t.Fatalf("create probe trigger: %v", err)
	}

	params := CreateUploadSessionParams{
		Filename: "loser.bin", FileSize: 4096, ChunkSize: 1024, TTLSeconds: 3600,
		EnvironmentID: "env-winner", RequestID: "req-winner",
		ClipExpiresAt: time.Now().Add(2 * time.Hour).UnixMilli(),
	}
	session, created, err := repo.CreateOrGetUploadSession(params)
	if err != nil {
		t.Fatalf("losing the insert race must be retried: %v", err)
	}
	if created {
		t.Fatalf("the retry must replay the winner, not mint a second session: %+v", session)
	}
	if session == nil || session.ID != winnerID || session.Filename != "winner.bin" {
		t.Fatalf("expected the winner session, got %+v", session)
	}
	if got := probeAttemptCount(t, repo); got != 1 {
		t.Fatalf("expected exactly one aborted insert attempt, got %d", got)
	}
	if got := sessionRowsWithKey(t, repo, "env-winner", "req-winner"); got != 1 {
		t.Fatalf("exactly one session may own the key, got %d", got)
	}
}

// TestCreateOrGetUploadSessionRetriesAreBounded pins the loop bound: when EVERY
// insert conflicts (here on an unrelated UNIQUE index, which is permanent), the
// call must give up after maxInitAttempts instead of spinning, and it must leave
// the repository usable.
func TestCreateOrGetUploadSessionRetriesAreBounded(t *testing.T) {
	repo := newTestRepository(t)
	seedInitProbe(t, repo)

	// A permanent conflict on filename: the key row does not exist, so the
	// lookup cannot resolve it, but the insert can never succeed either.
	if _, err := repo.db.Exec(
		"CREATE UNIQUE INDEX probe_unique_filename ON upload_sessions(filename)"); err != nil {
		t.Fatalf("create probe index: %v", err)
	}
	blocker := CreateUploadSessionParams{
		Filename: "bounded.bin", FileSize: 128, ChunkSize: 64, TTLSeconds: 3600,
		EnvironmentID: "env-bounded", RequestID: "req-blocker",
	}
	if _, _, err := repo.CreateOrGetUploadSession(blocker); err != nil {
		t.Fatalf("blocker session: %v", err)
	}
	// Count every insert attempt on the blocked filename.
	trigger := fmt.Sprintf(`CREATE TRIGGER probe_count_attempts BEFORE INSERT ON upload_sessions
		WHEN NEW.filename = 'bounded.bin'
		BEGIN
			INSERT INTO probe_attempts (n, at) VALUES (1, NEW.created_at);
			SELECT RAISE(FAIL, 'UNIQUE constraint failed: upload_sessions.filename');
		END`)
	if _, err := repo.db.Exec(trigger); err != nil {
		t.Fatalf("create probe trigger: %v", err)
	}

	params := CreateUploadSessionParams{
		Filename: "bounded.bin", FileSize: 512, ChunkSize: 256, TTLSeconds: 3600,
		EnvironmentID: "env-bounded", RequestID: "req-bounded",
		ClipExpiresAt: time.Now().Add(2 * time.Hour).UnixMilli(),
	}
	session, created, err := repo.CreateOrGetUploadSession(params)
	if err == nil {
		t.Fatalf("an unresolvable conflict must surface an error, got %+v", session)
	}
	if created {
		t.Fatalf("nothing may be created from a failing insert: %+v", session)
	}
	if got := probeAttemptCount(t, repo); got != maxInitAttempts {
		t.Fatalf("expected exactly maxInitAttempts (%d) insert attempts, got %d", maxInitAttempts, got)
	}
	if got := sessionRowsWithKey(t, repo, "env-bounded", "req-bounded"); got != 0 {
		t.Fatalf("no session may carry the failed key, got %d", got)
	}
	// The repository must still work (nothing leaked, no stuck transaction).
	healthy := blocker
	healthy.Filename = "healthy.bin"
	healthy.RequestID = "req-healthy"
	if _, created, err := repo.CreateOrGetUploadSession(healthy); err != nil || !created {
		t.Fatalf("repository must stay usable after bounded retries: created=%v err=%v", created, err)
	}
}

// TestCreateOrGetUploadSessionNonRetryableErrorsReturnImmediately: an insert
// failure that is NOT a UNIQUE violation must not be retried, and a key whose
// winner is mid-merge must be reported straight away (retrying it would only
// hammer a condition that cannot change).
func TestCreateOrGetUploadSessionNonRetryableErrorsReturnImmediately(t *testing.T) {
	repo := newTestRepository(t)
	seedInitProbe(t, repo)
	if _, err := repo.db.Exec(`CREATE TRIGGER probe_notnull BEFORE INSERT ON upload_sessions
		WHEN NEW.filename = 'readonly.bin'
		BEGIN
			INSERT INTO probe_attempts (n, at) VALUES (1, NEW.created_at);
			SELECT RAISE(FAIL, 'NOT NULL constraint failed: upload_sessions.mime_type');
		END`); err != nil {
		t.Fatalf("create probe trigger: %v", err)
	}

	_, _, err := repo.CreateOrGetUploadSession(CreateUploadSessionParams{
		Filename: "readonly.bin", FileSize: 100, ChunkSize: 50, TTLSeconds: 3600,
		EnvironmentID: "env-notnull", RequestID: "req-notnull",
		ClipExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	})
	if err == nil {
		t.Fatal("a non-UNIQUE insert failure must be surfaced")
	}
	if got := probeAttemptCount(t, repo); got != 1 {
		t.Fatalf("a non-retryable failure must be attempted exactly once, got %d", got)
	}

	// A `completing` winner whose TTL elapsed: refused immediately, no retry.
	stale := CreateUploadSessionParams{
		Filename: "merge.bin", FileSize: 100, ChunkSize: 100, TTLSeconds: 3600,
		EnvironmentID: "env-merge", RequestID: "req-merge",
		ClipExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}
	created, _, err := repo.CreateOrGetUploadSession(stale)
	if err != nil {
		t.Fatalf("seed stale session: %v", err)
	}
	if err := repo.MarkChunkReceived(created.ID, 0, 100); err != nil {
		t.Fatalf("mark chunk: %v", err)
	}
	if _, _, err := repo.TryBeginComplete(created.ID,
		filepath.Join(repo.Settings().FileStorageDir, "merge.bin")); err != nil {
		t.Fatalf("begin complete: %v", err)
	}
	if err := repo.SetUploadExpiryForTest(created.ID, time.Now().Add(-time.Minute).Unix()); err != nil {
		t.Fatalf("expire: %v", err)
	}
	session, createdFlag, err := repo.CreateOrGetUploadSession(stale)
	var unavailable *SessionUnavailableError
	if err == nil || !errors.As(err, &unavailable) {
		t.Fatalf("expected SessionUnavailableError, got session=%+v created=%v err=%v",
			session, createdFlag, err)
	}
	if unavailable.Session == nil || unavailable.Session.ID != created.ID {
		t.Fatalf("refusal must carry the blocking session, got %+v", unavailable.Session)
	}
	_ = os.Remove(filepath.Join(repo.Settings().FileStorageDir, "merge.bin"))
}
