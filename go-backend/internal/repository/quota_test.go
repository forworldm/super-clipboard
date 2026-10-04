package repository

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pixia1234/super-clipboard/backend/internal/apperr"
	"github.com/pixia1234/super-clipboard/backend/internal/config"
)

func reservedBytes(t *testing.T, repo *ClipRepository) int64 {
	t.Helper()
	value, err := repo.UploadQuotaValue()
	if err != nil {
		t.Fatalf("unable to read upload quota: %v", err)
	}
	if value < 0 {
		t.Fatalf("reserved bytes must never be negative, got %d", value)
	}
	return value
}

// createReservingSession creates a session (which holds a fileSize reservation)
// and asserts the ledger moved by exactly fileSize.
func createReservingSession(t *testing.T, repo *ClipRepository, name string, fileSize int64, before int64) *UploadSession {
	t.Helper()
	session, err := repo.CreateUploadSession(CreateUploadSessionParams{
		Filename: name, FileSize: fileSize, MimeType: "application/octet-stream",
		EnvironmentID: "env-quota", ChunkSize: 1024, TTLSeconds: 3600,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := repo.ReserveUploadQuota(fileSize, 0); err != nil {
		t.Fatalf("reserve quota: %v", err)
	}
	if got := reservedBytes(t, repo); got != before+fileSize {
		t.Fatalf("reserved = %d, want %d", got, before+fileSize)
	}
	return session
}

// TestUploadQuotaReserveBoundary exercises the exact budget edge: a reservation
// landing precisely on the limit succeeds, one more byte is refused and must
// not move the ledger.
func TestUploadQuotaReserveBoundary(t *testing.T) {
	repo := newTestRepository(t)
	const limit = 1000

	if err := repo.ReserveUploadQuota(400, limit); err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	if err := repo.ReserveUploadQuota(600, limit); err != nil {
		t.Fatalf("reserve to exactly the limit must succeed: %v", err)
	}
	if got := reservedBytes(t, repo); got != limit {
		t.Fatalf("reserved = %d, want %d", got, limit)
	}

	err := repo.ReserveUploadQuota(1, limit)
	var storageErr *apperr.StorageError
	if !errors.As(err, &storageErr) {
		t.Fatalf("expected a typed StorageError, got %v", err)
	}
	if storageErr.Code != apperr.StorageCodeQuota {
		t.Fatalf("unexpected code %q", storageErr.Code)
	}
	if storageErr.Used != limit || storageErr.Requested != 1 || storageErr.Limit != limit {
		t.Fatalf("unexpected error payload: %+v", storageErr)
	}
	if got := reservedBytes(t, repo); got != limit {
		t.Fatalf("a refused reservation must not move the ledger: %d", got)
	}

	// A zero-byte reservation is free but still legal at the boundary.
	if err := repo.ReserveUploadQuota(0, limit); err != nil {
		t.Fatalf("zero-byte reserve at the limit: %v", err)
	}
}

// TestUploadQuotaConcurrentReserveNoOversell is the no-oversell guarantee:
// N goroutines racing for a budget that fits M of them must let exactly M in.
func TestUploadQuotaConcurrentReserveNoOversell(t *testing.T) {
	repo := newTestRepository(t)
	const (
		limit      = 1000
		perRequest = 100
		wantWins   = limit / perRequest
		attempts   = 40
	)

	var wins int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := repo.ReserveUploadQuota(perRequest, limit); err == nil {
				atomic.AddInt64(&wins, 1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt64(&wins); got != wantWins {
		t.Fatalf("oversell: %d reservations succeeded, want exactly %d", got, wantWins)
	}
	if got := reservedBytes(t, repo); got != limit {
		t.Fatalf("reserved = %d, want %d", got, limit)
	}
}

// TestUploadQuotaReleaseExactlyOnce proves the release CAS: many concurrent
// releases of one session return the bytes a single time.
func TestUploadQuotaReleaseExactlyOnce(t *testing.T) {
	repo := newTestRepository(t)
	session := createReservingSession(t, repo, "once.bin", 500, 0)

	var released int64
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := repo.ReleaseUploadQuota(session.ID, session.FileSize)
			if err != nil {
				t.Errorf("release: %v", err)
				return
			}
			if ok {
				atomic.AddInt64(&released, 1)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&released); got != 1 {
		t.Fatalf("quota returned %d times, want exactly 1", got)
	}
	if got := reservedBytes(t, repo); got != 0 {
		t.Fatalf("reserved = %d, want 0", got)
	}
}

// TestUploadQuotaConcurrentAbortPurgeComplete races abort, expiry cleanup and
// completion on the same session. The bytes must come back exactly once and the
// ledger must never dip below zero (a watcher samples it during the race).
func TestUploadQuotaConcurrentAbortPurgeComplete(t *testing.T) {
	repo := newTestRepository(t)
	// Sentinel: an untouched reservation. A double release of the racing
	// session would drag the balance below the sentinel, so the final balance
	// proves the bytes were returned exactly once.
	sentinel := createReservingSession(t, repo, "sentinel.bin", 5000, 0)
	session := createReservingSession(t, repo, "race.bin", 700, 5000)
	// Make it expire "now" so the purge path picks it up as well.
	if err := repo.SetUploadExpiryForTest(session.ID, time.Now().UTC().Unix()); err != nil {
		t.Fatalf("force expiry: %v", err)
	}

	var (
		minimum atomic.Int64
		wg      sync.WaitGroup
		stop    = make(chan struct{})
	)
	minimum.Store(1 << 62)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			value, err := repo.UploadQuotaValue()
			if err != nil {
				continue
			}
			for {
				current := minimum.Load()
				if value >= current || minimum.CompareAndSwap(current, value) {
					break
				}
			}
		}
	}()

	var wins int64
	mark := func(ok bool) {
		if ok {
			atomic.AddInt64(&wins, 1)
		}
	}
	var actors sync.WaitGroup
	for i := 0; i < 6; i++ {
		actors.Add(3)
		go func() {
			defer actors.Done()
			_, err := repo.AbortUploadSession(session.ID)
			mark(err == nil)
		}()
		go func() {
			defer actors.Done()
			victims, err := repo.PurgeExpiredUploads(time.Now().UTC().Unix() + 1)
			mark(err == nil && len(victims) > 0)
		}()
		go func() {
			defer actors.Done()
			released, err := repo.ReleaseUploadQuota(session.ID, session.FileSize)
			mark(released && err == nil)
		}()
	}
	actors.Wait()
	close(stop)
	wg.Wait()

	if lowest := minimum.Load(); lowest < 0 {
		t.Fatalf("reserved bytes went negative (%d) during the race", lowest)
	}
	if atomic.LoadInt64(&wins) == 0 {
		t.Fatal("no actor returned the reservation")
	}
	// Exactly one decrement of 700: anything else would leave the balance away
	// from the untouched sentinel reservation.
	if got := reservedBytes(t, repo); got != sentinel.FileSize {
		t.Fatalf("reserved = %d, want exactly the %d byte sentinel (reservation must return once)",
			got, sentinel.FileSize)
	}
}

// TestUploadQuotaReleasedOnComplete covers option A: a successful complete
// gives the reservation back in the same transaction.
func TestUploadQuotaReleasedOnComplete(t *testing.T) {
	repo := newTestRepository(t)
	session := createReservingSession(t, repo, "done.bin", 2048, 0)
	for i := 0; i < session.TotalChunks; i++ {
		if err := repo.MarkChunkReceived(session.ID, i, 1024); err != nil {
			t.Fatalf("mark chunk: %v", err)
		}
	}
	completing, _, err := repo.TryBeginComplete(session.ID)
	if err != nil {
		t.Fatalf("begin complete: %v", err)
	}
	completed, err := repo.CompleteUploadSession(completing.ID, "")
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if !completed.QuotaReleased {
		t.Fatal("completed session must be flagged quota_released")
	}
	if got := reservedBytes(t, repo); got != 0 {
		t.Fatalf("reserved = %d after complete, want 0", got)
	}
	// A second release attempt (abort of the finished row, retry, ...) is a no-op.
	released, err := repo.ReleaseUploadQuota(completed.ID, completed.FileSize)
	if err != nil {
		t.Fatalf("second release: %v", err)
	}
	if released {
		t.Fatal("quota must only be returned once")
	}
	if got := reservedBytes(t, repo); got != 0 {
		t.Fatalf("reserved = %d, want 0", got)
	}
}

// TestFailCompleteKeepsReservationUntilTerminal: the complete rollback path
// returns the session to `active` with its chunks still on disk, so the bytes
// MUST stay reserved (otherwise they would occupy disk unaccounted for). The
// reservation comes back exactly once on the terminal transition (abort).
func TestFailCompleteKeepsReservationUntilTerminal(t *testing.T) {
	repo := newTestRepository(t)
	session := createReservingSession(t, repo, "fail.bin", 512, 0)
	if err := repo.MarkChunkReceived(session.ID, 0, 512); err != nil {
		t.Fatalf("mark chunk: %v", err)
	}
	if _, _, err := repo.TryBeginComplete(session.ID); err != nil {
		t.Fatalf("begin complete: %v", err)
	}
	if _, err := repo.FailComplete(session.ID); err != nil {
		t.Fatalf("fail complete: %v", err)
	}
	if got := reservedBytes(t, repo); got != 512 {
		t.Fatalf("reserved = %d after rollback, want the 512 bytes kept", got)
	}
	// The rolled-back session is resumable: a retry releases nothing extra.
	if _, err := repo.FailComplete(session.ID); err != nil {
		t.Fatalf("second fail complete: %v", err)
	}
	if got := reservedBytes(t, repo); got != 512 {
		t.Fatalf("reserved = %d, want 512 (rollback idempotent)", got)
	}
	if _, err := repo.AbortUploadSession(session.ID); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if got := reservedBytes(t, repo); got != 0 {
		t.Fatalf("reserved = %d after abort, want 0", got)
	}
	released, err := repo.ReleaseUploadQuota(session.ID, session.FileSize)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if released {
		t.Fatal("abort already returned the reservation")
	}
}

// TestActiveUploadSessionCount is the counter behind MaxActiveUploadSessions.
func TestActiveUploadSessionCount(t *testing.T) {
	repo := newTestRepository(t)
	for i := 0; i < 3; i++ {
		if _, err := repo.CreateUploadSession(CreateUploadSessionParams{
			Filename: fmt.Sprintf("s%d.bin", i), FileSize: 10, MimeType: "application/octet-stream",
			EnvironmentID: "env-count", ChunkSize: 1024, TTLSeconds: 3600,
		}); err != nil {
			t.Fatalf("create session: %v", err)
		}
	}
	count, err := repo.CountActiveUploadSessions()
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 3 {
		t.Fatalf("active sessions = %d, want 3", count)
	}
	if _, err := repo.AbortUploadSession(mustListOne(t, repo).ID); err != nil {
		t.Fatalf("abort: %v", err)
	}
	count, err = repo.CountActiveUploadSessions()
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("active sessions = %d after abort, want 2", count)
	}
}

func mustListOne(t *testing.T, repo *ClipRepository) *UploadSession {
	t.Helper()
	sessions, err := repo.ListUploadSessions()
	if err != nil || len(sessions) == 0 {
		t.Fatalf("unable to list sessions: %v (%d)", err, len(sessions))
	}
	return sessions[0]
}

// TestRecomputeUploadQuotaFixesDrift: the cleanup worker overwrites the ledger
// with SUM(file_size) of active, unreleased sessions.
func TestRecomputeUploadQuotaFixesDrift(t *testing.T) {
	repo := newTestRepository(t)
	createReservingSession(t, repo, "a.bin", 300, 0)
	createReservingSession(t, repo, "b.bin", 700, 300)
	if got := reservedBytes(t, repo); got != 1000 {
		t.Fatalf("reserved = %d, want 1000", got)
	}

	if err := repo.ForceUploadQuotaForTest(424242); err != nil {
		t.Fatalf("inject drift: %v", err)
	}
	recomputed, previous, err := repo.RecomputeUploadQuota()
	if err != nil {
		t.Fatalf("recompute: %v", err)
	}
	if previous != 424242 || recomputed != 1000 {
		t.Fatalf("recompute = %d (previous %d), want 1000 (previous 424242)", recomputed, previous)
	}
	if got := reservedBytes(t, repo); got != 1000 {
		t.Fatalf("reserved = %d after reconciliation, want 1000", got)
	}
}

// TestRecomputeIgnoresReleasedAndCompletedSessions: only active sessions that
// still hold their reservation are summed.
func TestRecomputeIgnoresReleasedAndCompletedSessions(t *testing.T) {
	repo := newTestRepository(t)
	kept := createReservingSession(t, repo, "kept.bin", 250, 0)
	released := createReservingSession(t, repo, "released.bin", 4000, 250)
	if _, err := repo.ReleaseUploadQuota(released.ID, released.FileSize); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := repo.SetUploadExpiryForTest(kept.ID, time.Now().UTC().Unix()+3600); err != nil {
		t.Fatalf("set expiry: %v", err)
	}
	recomputed, _, err := repo.RecomputeUploadQuota()
	if err != nil {
		t.Fatalf("recompute: %v", err)
	}
	if recomputed != 250 {
		t.Fatalf("recomputed = %d, want 250", recomputed)
	}
}

// TestLegacyDatabaseMigratesUploadQuota opens a database written by the
// pre-quota schema and verifies the migration creates upload_quota, seeds it
// from the existing active sessions and flags their quota_released as 0.
func TestLegacyDatabaseMigratesUploadQuota(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	statements := []string{
		`CREATE TABLE upload_sessions (
			id TEXT PRIMARY KEY, filename TEXT NOT NULL, file_size INTEGER NOT NULL,
			mime_type TEXT NOT NULL, chunk_size INTEGER NOT NULL, total_chunks INTEGER NOT NULL,
			status TEXT NOT NULL, environment_id TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, expires_at INTEGER NOT NULL,
			staged_path TEXT, clip_id TEXT
		)`,
		`INSERT INTO upload_sessions VALUES ('old-1','old.bin',777,'application/octet-stream',1024,1,'active','env-old',1,1,9999999999,NULL,NULL)`,
		`INSERT INTO upload_sessions VALUES ('old-2','gone.bin',111,'application/octet-stream',1024,1,'completed','env-old',1,1,9999999999,NULL,NULL)`,
	}
	for _, statement := range statements {
		if _, err := legacy.Exec(statement); err != nil {
			t.Fatalf("legacy schema: %v", err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	settings := config.Defaults()
	settings.DatabasePath = path
	settings.FileStorageDir = filepath.Join(dir, "files")
	repo, err := NewClipRepository(settings)
	if err != nil {
		t.Fatalf("migrate legacy db: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	session, err := repo.GetUploadSession("old-1")
	if err != nil || session == nil {
		t.Fatalf("load legacy session: %v", err)
	}
	if session.QuotaReleased {
		t.Fatal("legacy sessions must default to quota_released = 0")
	}
	if got := reservedBytes(t, repo); got != 777 {
		t.Fatalf("seeded reserved = %d, want 777 (only active, unreleased sessions)", got)
	}
}

// TestCheckDiskWatermark is the pure guard behind both the init and chunk gates.
func TestCheckDiskWatermark(t *testing.T) {
	if err := CheckDiskWatermark(0, 0); err != nil {
		t.Fatalf("guard disabled must pass: %v", err)
	}
	if err := CheckDiskWatermark(1<<20, 1<<20); err != nil {
		t.Fatalf("exactly at the watermark must pass: %v", err)
	}
	err := CheckDiskWatermark(1<<20-1, 1<<20)
	var storageErr *apperr.StorageError
	if !errors.As(err, &storageErr) {
		t.Fatalf("expected typed StorageError, got %v", err)
	}
	if storageErr.Code != apperr.StorageCodeDisk {
		t.Fatalf("unexpected code %q", storageErr.Code)
	}
	if storageErr.HTTPStatus() != 507 {
		t.Fatalf("disk watermark must answer 507, got %d", storageErr.HTTPStatus())
	}
}
