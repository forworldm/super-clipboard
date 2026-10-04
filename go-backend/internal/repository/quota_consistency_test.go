package repository

// Consistency tests for the upload-quota ledger.
//
// The ledger invariant under test is:
//
//	upload_quota.reserved_bytes == SUM(file_size)
//	    FROM upload_sessions WHERE quota_released = 0
//
// i.e. the reservation flag ALONE decides whether a session's bytes are
// reserved. `status` is a separate state machine and must never appear in that
// query: a session in `completing` still owns its reservation (the flip is taken
// before the clip INSERT; the release happens in the terminal transition), so a
// `status = 'active'` filter under-counts and -- worse -- lets
// RecomputeUploadQuota "repair" the missing amount away, handing the same bytes
// to the next init while the original session still holds its file.
//
// These tests are the regression lock for that class of bug, for every state a
// session can be in and for the cleanup paths that reclaim rows.

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/pixia1234/super-clipboard/backend/internal/config"
	"github.com/pixia1234/super-clipboard/backend/internal/models"
)

// assertLedgerInvariant checks the ledger against the reservation holders and
// returns the number of holders.
func assertLedgerInvariant(t *testing.T, repo *ClipRepository, want int64, context string) int64 {
	t.Helper()
	ledger, expected, err := repo.UploadQuotaDrift()
	if err != nil {
		t.Fatalf("%s: drift probe: %v", context, err)
	}
	if ledger != want {
		t.Fatalf("%s: reserved_bytes = %d, want %d", context, ledger, want)
	}
	if expected != want {
		t.Fatalf("%s: SUM(file_size) of reservation holders = %d, want %d", context, expected, want)
	}
	holders, err := repo.CountUploadSessionsHoldingQuota()
	if err != nil {
		t.Fatalf("%s: holder count: %v", context, err)
	}
	return holders
}

// expireAndAge drives a session row past its TTL and (optionally) ages
// updated_at, which is what the `completing` purge grace is measured against.
func expireAndAge(t *testing.T, repo *ClipRepository, uploadID string, expired, ageSeconds int64) {
	t.Helper()
	now := time.Now().UTC().Unix()
	if _, err := repo.db.Exec(
		"UPDATE upload_sessions SET expires_at = ?, updated_at = ? WHERE id = ?",
		now-expired, now-ageSeconds, uploadID); err != nil {
		t.Fatalf("age session %s: %v", uploadID, err)
	}
}

// TestLedgerCountsCompletingSessions is THE regression test for the reported
// bug: a session flipped to `completing` still holds its reservation, so the
// ledger, the recompute repair and the session-cap counter must all count it.
func TestLedgerCountsCompletingSessions(t *testing.T) {
	repo := newTestRepository(t)
	active := createReservingSession(t, repo, "active.bin", 400, 0)
	inflight := createReservingSession(t, repo, "inflight.bin", 600, 400)
	if err := repo.MarkChunkReceived(inflight.ID, 0, 600); err != nil {
		t.Fatalf("mark chunk: %v", err)
	}

	completing, _, err := repo.TryBeginComplete(inflight.ID)
	if err != nil {
		t.Fatalf("begin complete: %v", err)
	}
	if completing.Status != UploadStatusCompleting {
		t.Fatalf("status = %q, want completing", completing.Status)
	}

	// The flip is NOT a release: 400 (active) + 600 (completing) stays reserved.
	if holders := assertLedgerInvariant(t, repo, 1000, "while completing"); holders != 2 {
		t.Fatalf("reservation holders = %d, want 2 (active + completing)", holders)
	}
	// The recompute repair must agree with the ledger -- a status filter would
	// have answered 400 here and then OVERWRITTEN the ledger with it.
	if recomputed, previous, err := repo.RecomputeUploadQuota(); err != nil || recomputed != 1000 || previous != 1000 {
		t.Fatalf("recompute = %d (previous %d, err %v), want 1000/1000: the completing session must not be dropped",
			recomputed, previous, err)
	}
	assertLedgerInvariant(t, repo, 1000, "after recompute")

	// `active` and "holds a reservation" are deliberately different counters.
	activeCount, err := repo.CountActiveUploadSessions()
	if err != nil {
		t.Fatalf("active count: %v", err)
	}
	if activeCount != 1 {
		t.Fatalf("active sessions = %d, want 1 (completing is not active)", activeCount)
	}

	// Success releases exactly the completing session's bytes, in the same
	// transaction as the completing->completed flip.
	if _, err := repo.CompleteUploadSession(completing.ID, "clip-inflight"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if holders := assertLedgerInvariant(t, repo, 400, "after complete"); holders != 1 {
		t.Fatalf("reservation holders = %d, want 1", holders)
	}

	// The terminal cancel of the remaining session drains the ledger.
	if _, err := repo.AbortUploadSession(active.ID); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if holders := assertLedgerInvariant(t, repo, 0, "after abort"); holders != 0 {
		t.Fatalf("reservation holders = %d, want 0", holders)
	}
	// A finished session row may still exist (idempotency record) but must never
	// count towards the ledger again.
	if recomputed, _, err := repo.RecomputeUploadQuota(); err != nil || recomputed != 0 {
		t.Fatalf("recompute = %d (err %v), want 0: a completed session is released", recomputed, err)
	}
}

// TestLedgerInvariantAcrossEveryTransition walks one session through the whole
// state machine and asserts the invariant (and the recompute repair) after every
// single step, including the artificial drift injection in the middle.
func TestLedgerInvariantAcrossEveryTransition(t *testing.T) {
	repo := newTestRepository(t)
	session, err := repo.CreateUploadSession(CreateUploadSessionParams{
		Filename: "walk.bin", FileSize: 2048, MimeType: "application/octet-stream",
		EnvironmentID: "env-walk", ChunkSize: 1024, TTLSeconds: 3600,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	steps := []struct {
		name string
		run  func()
		hold int64
	}{
		{"after init reservation", func() {
			if err := repo.ReserveUploadQuota(session.FileSize, 0); err != nil {
				t.Fatalf("reserve: %v", err)
			}
		}, 2048},
		{"after first chunk", func() {
			if err := repo.MarkChunkReceived(session.ID, 0, 1024); err != nil {
				t.Fatalf("mark chunk 0: %v", err)
			}
		}, 2048},
		{"after last chunk", func() {
			if err := repo.MarkChunkReceived(session.ID, 1, 1024); err != nil {
				t.Fatalf("mark chunk 1: %v", err)
			}
		}, 2048},
		{"active->completing", func() {
			if _, _, err := repo.TryBeginComplete(session.ID); err != nil {
				t.Fatalf("begin complete: %v", err)
			}
		}, 2048},
		{"completing->active (rollback)", func() {
			if _, err := repo.FailComplete(session.ID); err != nil {
				t.Fatalf("fail complete: %v", err)
			}
		}, 2048},
	}
	for _, step := range steps {
		step.run()
		assertLedgerInvariant(t, repo, step.hold, step.name)
		if recomputed, _, err := repo.RecomputeUploadQuota(); err != nil || recomputed != step.hold {
			t.Fatalf("%s: recompute = %d (err %v), want %d", step.name, recomputed, err, step.hold)
		}
	}

	// Drift injection: the reconciliation must repair towards the holders, not
	// towards a status-filtered subset.
	if err := repo.ForceUploadQuotaForTest(99); err != nil {
		t.Fatalf("inject drift: %v", err)
	}
	if recomputed, previous, err := repo.RecomputeUploadQuota(); err != nil || recomputed != 2048 || previous != 99 {
		t.Fatalf("repair = %d (previous %d, err %v), want 2048/99", recomputed, previous, err)
	}
	assertLedgerInvariant(t, repo, 2048, "after repair")

	if err := repo.MarkChunkReceived(session.ID, 1, 1024); err != nil {
		t.Fatalf("mark chunk 1: %v", err)
	}
	if _, _, err := repo.TryBeginComplete(session.ID); err != nil {
		t.Fatalf("second begin complete: %v", err)
	}
	assertLedgerInvariant(t, repo, 2048, "completing again")
	if _, err := repo.CompleteUploadSession(session.ID, "clip-walk"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	assertLedgerInvariant(t, repo, 0, "completing->completed")
	if recomputed, _, err := repo.RecomputeUploadQuota(); err != nil || recomputed != 0 {
		t.Fatalf("recompute = %d (err %v), want 0", recomputed, err)
	}
}

// TestFreshCompletingRowSurvivesPurgeWithoutLosingQuota covers the other half of
// the bug: the expiry worker protects a `completing` row that may still be
// inserting its clip, and the protected row's bytes stay reserved -- the repair
// must never free them behind its back.
func TestFreshCompletingRowSurvivesPurgeWithoutLosingQuota(t *testing.T) {
	repo := newTestRepository(t)
	session := createReservingSession(t, repo, "fresh.bin", 555, 0)
	if err := repo.MarkChunkReceived(session.ID, 0, 555); err != nil {
		t.Fatalf("mark chunk: %v", err)
	}
	if _, _, err := repo.TryBeginComplete(session.ID); err != nil {
		t.Fatalf("begin complete: %v", err)
	}
	// The TTL elapsed during the (bounded) COMPLETE sequence: updated_at is still
	// fresh, so the purge grace protects the row.
	expireAndAge(t, repo, session.ID, 60, 0)

	now := time.Now().UTC().Unix()
	victims, err := repo.PurgeExpiredUploads(now)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if len(victims) != 0 {
		t.Fatalf("an in-flight completing session must survive the purge, got %+v", victims)
	}
	if cur, _ := repo.GetUploadSession(session.ID); cur == nil || cur.Status != UploadStatusCompleting {
		t.Fatalf("completing session must survive: %+v", cur)
	}
	// Regression: the recompute repair must keep the protected bytes reserved.
	// (A status='active' filter zeroed the ledger here while the row stayed alive
	// and kept its file -- free credit for the next init.)
	assertLedgerInvariant(t, repo, 555, "protected completing row")
	if recomputed, _, err := repo.RecomputeUploadQuota(); err != nil || recomputed != 555 {
		t.Fatalf("recompute = %d (err %v), want 555 kept", recomputed, err)
	}
}

// TestPurgeReclaimsStaleCompletingAfterGrace: a `completing` row stuck by a
// hard kill is reclaimed once it outlived the grace window, and the reservation
// comes back exactly once (the row disappear plus the ledger drop are atomic).
func TestPurgeReclaimsStaleCompletingAfterGrace(t *testing.T) {
	repo := newTestRepository(t)
	session := createReservingSession(t, repo, "stuck.bin", 900, 0)
	if err := repo.MarkChunkReceived(session.ID, 0, 900); err != nil {
		t.Fatalf("mark chunk: %v", err)
	}
	if _, _, err := repo.TryBeginComplete(session.ID); err != nil {
		t.Fatalf("begin complete: %v", err)
	}
	age := CompletingPurgeGraceSeconds + 60
	expireAndAge(t, repo, session.ID, 600, age)

	now := time.Now().UTC().Unix()
	victims, err := repo.PurgeExpiredUploads(now)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if len(victims) != 1 || victims[0].UploadID != session.ID {
		t.Fatalf("stale completing row must be purged, got %+v", victims)
	}
	if victims[0].HasClip {
		t.Fatalf("a stuck session has no clip: %+v", victims[0])
	}
	if cur, _ := repo.GetUploadSession(session.ID); cur != nil {
		t.Fatalf("purged session row must be gone, got %+v", cur)
	}
	assertLedgerInvariant(t, repo, 0, "after stale completing purge")
	if recomputed, _, err := repo.RecomputeUploadQuota(); err != nil || recomputed != 0 {
		t.Fatalf("recompute = %d (err %v), want 0", recomputed, err)
	}
	// The reservation is not returned twice by a second cleanup pass.
	if victims, err := repo.PurgeExpiredUploads(now); err != nil || len(victims) != 0 {
		t.Fatalf("second purge: victims=%+v err=%v, want none", victims, err)
	}
	if released, err := repo.ReleaseUploadQuota(session.ID, session.FileSize); err != nil || released {
		t.Fatalf("release after purge: released=%v err=%v, want false/nil", released, err)
	}
	assertLedgerInvariant(t, repo, 0, "after extra release attempt")

	// The grace window is honoured exactly: the same row is NOT eligible one
	// second early.
	second := createReservingSession(t, repo, "borderline.bin", 300, 0)
	if err := repo.MarkChunkReceived(second.ID, 0, 300); err != nil {
		t.Fatalf("mark chunk: %v", err)
	}
	if _, _, err := repo.TryBeginComplete(second.ID); err != nil {
		t.Fatalf("begin complete: %v", err)
	}
	expireAndAge(t, repo, second.ID, 600, CompletingPurgeGraceSeconds-30)
	if victims, err := repo.PurgeExpiredUploads(now); err != nil || len(victims) != 0 {
		t.Fatalf("inside the grace window: victims=%+v err=%v, want none", victims, err)
	}
	assertLedgerInvariant(t, repo, 300, "inside the grace window")
}

// TestPurgeKeepsContainerOfCompletingRowThatAlreadyHasClip covers the
// interleaving the grace window cannot cover on its own: the clip row was
// already inserted (so the container is now owned by a clip) but the
// completing->completed flip has not landed. The victim must report HasClip so
// the file survives the purge.
func TestPurgeKeepsContainerOfCompletingRowThatAlreadyHasClip(t *testing.T) {
	repo := newTestRepository(t)
	session := createReservingSession(t, repo, "halfway.bin", 700, 0)
	if err := repo.MarkChunkReceived(session.ID, 0, 700); err != nil {
		t.Fatalf("mark chunk: %v", err)
	}
	if _, _, err := repo.TryBeginComplete(session.ID); err != nil {
		t.Fatalf("begin complete: %v", err)
	}
	// Sessions created straight through the repository carry no container (only
	// INIT pre-allocates it); this test is about the path, so set it explicitly.
	staged := filepath.Join(t.TempDir(), "halfway.bin")
	if _, err := repo.db.Exec("UPDATE upload_sessions SET staged_path = ? WHERE id = ?", staged, session.ID); err != nil {
		t.Fatalf("set staged path: %v", err)
	}
	// The clip INSERT of that very COMPLETE, already committed.
	clip, err := repo.CreateClip(CreateClipParams{
		ClipType: models.ClipTypeFile, ExpiresAtMs: time.Now().Add(time.Hour).UnixMilli(),
		EnvironmentID: "env-quota",
		StoredFile: &models.StoredFile{
			Name: session.Filename, Size: session.FileSize, Mime: session.MimeType,
			Path: staged,
		},
	})
	if err != nil {
		t.Fatalf("create clip: %v", err)
	}
	if clip.StoredFile == nil || clip.StoredFile.Path != staged {
		t.Fatalf("clip must point at the staged container: %+v", clip.StoredFile)
	}
	expireAndAge(t, repo, session.ID, 600, CompletingPurgeGraceSeconds+60)

	victims, err := repo.PurgeExpiredUploads(time.Now().UTC().Unix())
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if len(victims) != 1 || !victims[0].HasClip {
		t.Fatalf("the container is owned by a clip now, victim must say so: %+v", victims)
	}
	// The row is gone (its reservation is returned) and the file is kept for the
	// clip: the handler only unlinks victims without a clip.
	assertLedgerInvariant(t, repo, 0, "after purge with clip-backed container")
}

// TestLegacyDatabaseQuotaSeedCountsEveryHolder covers the upgrade path: the
// seed must add up every session that still holds a reservation -- `active` AND
// `completing` -- and must release the rows that certainly do not (`completed`),
// because the freshly added flag is backfilled to 0 for all of them.
func TestLegacyDatabaseQuotaSeedCountsEveryHolder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy-holders.db")
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
		`INSERT INTO upload_sessions VALUES ('old-active','active.bin',777,'application/octet-stream',1024,1,'active','env-old',1,1,9999999999,'/files/a','')`,
		`INSERT INTO upload_sessions VALUES ('old-completing','flight.bin',555,'application/octet-stream',1024,1,'completing','env-old',1,1,9999999999,'/files/b','')`,
		`INSERT INTO upload_sessions VALUES ('old-completed','done.bin',111,'application/octet-stream',1024,1,'completed','env-old',1,1,9999999999,'/files/c','clip-1')`,
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

	// 777 (active) + 555 (completing) = 1332; the finished row contributes
	// nothing because it released its reservation when it completed.
	assertLedgerInvariant(t, repo, 1332, "legacy seed")
	for _, id := range []string{"old-active", "old-completing"} {
		session, err := repo.GetUploadSession(id)
		if err != nil || session == nil {
			t.Fatalf("load %s: %v", id, err)
		}
		if session.QuotaReleased {
			t.Fatalf("%s still holds its reservation, it must NOT be flagged released", id)
		}
	}
	done, err := repo.GetUploadSession("old-completed")
	if err != nil || done == nil {
		t.Fatalf("load completed session: %v", err)
	}
	if !done.QuotaReleased {
		t.Fatal("a legacy completed session must be flagged released by the migration")
	}
	// The seeded ledger is the convergent value, so the first repair is a no-op.
	if recomputed, previous, err := repo.RecomputeUploadQuota(); err != nil || recomputed != 1332 || previous != 1332 {
		t.Fatalf("recompute after migration = %d (previous %d, err %v), want 1332/1332", recomputed, previous, err)
	}
}

// TestReservationHolderCounterDrivesTheSessionCap documents the counter the
// MaxActiveUploadSessions gate uses: it must include `completing` sessions,
// because those still own a reserved slot.
func TestReservationHolderCounterDrivesTheSessionCap(t *testing.T) {
	repo := newTestRepository(t)
	first := createReservingSession(t, repo, fmt.Sprintf("cap-%d.bin", 0), 128, 0)
	if err := repo.MarkChunkReceived(first.ID, 0, 128); err != nil {
		t.Fatalf("mark chunk: %v", err)
	}
	if _, _, err := repo.TryBeginComplete(first.ID); err != nil {
		t.Fatalf("begin complete: %v", err)
	}
	holders, err := repo.CountUploadSessionsHoldingQuota()
	if err != nil {
		t.Fatalf("holders: %v", err)
	}
	if holders != 1 {
		t.Fatalf("holders = %d, want 1 (the completing session still owns a slot)", holders)
	}
	active, err := repo.CountActiveUploadSessions()
	if err != nil {
		t.Fatalf("active: %v", err)
	}
	if active != 0 {
		t.Fatalf("active = %d, want 0: status and reservation are different questions", active)
	}
	// Once the session finishes, both counters agree that the slot is free.
	if _, err := repo.CompleteUploadSession(first.ID, "clip-cap"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if holders, _ := repo.CountUploadSessionsHoldingQuota(); holders != 0 {
		t.Fatalf("holders = %d after complete, want 0", holders)
	}
}
