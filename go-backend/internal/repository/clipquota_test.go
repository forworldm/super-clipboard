package repository

import (
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pixia1234/super-clipboard/backend/internal/apperr"
	"github.com/pixia1234/super-clipboard/backend/internal/config"
	"github.com/pixia1234/super-clipboard/backend/internal/models"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// fileValue describes a stored file without touching the disk: the ledger only
// cares about the declared size, which is exactly what the upload/assembly code
// froze on the session before calling CreateClip.
func fileValue(name string, size int64) *models.StoredFile {
	return &models.StoredFile{Name: name, Size: size, Mime: "application/octet-stream", Path: ""}
}

func createFileClip(t *testing.T, repo *ClipRepository, name string, size int64, owner string) *models.Clip {
	t.Helper()
	clip, err := repo.CreateClip(CreateClipParams{
		ClipType:      models.ClipTypeFile,
		ExpiresAtMs:   time.Now().Add(time.Hour).UnixMilli(),
		EnvironmentID: owner,
		StoredFile:    fileValue(name, size),
	})
	if err != nil {
		t.Fatalf("unable to create file clip %s: %v", name, err)
	}
	return clip
}

// storedBytes reads the ledger and asserts the invariant "never negative".
func storedBytes(t *testing.T, repo *ClipRepository) int64 {
	t.Helper()
	value, err := repo.ClipQuotaValue()
	if err != nil {
		t.Fatalf("unable to read clip quota: %v", err)
	}
	if value < 0 {
		t.Fatalf("used bytes must never be negative, got %d", value)
	}
	return value
}

func countClips(t *testing.T, repo *ClipRepository) int64 {
	t.Helper()
	var total int64
	if err := repo.db.QueryRow("SELECT COUNT(1) FROM clips").Scan(&total); err != nil {
		t.Fatalf("unable to count clips: %v", err)
	}
	return total
}

// assertClipQuotaError checks the typed refusal raised by the insert CAS.
func assertClipQuotaError(t *testing.T, err error, requested int64, used int64, limit int64) {
	t.Helper()
	var storageErr *apperr.StorageError
	if !errors.As(err, &storageErr) {
		t.Fatalf("expected a typed StorageError, got %v", err)
	}
	if storageErr.Code != apperr.StorageCodeClipQuota {
		t.Fatalf("unexpected code %q, want %q", storageErr.Code, apperr.StorageCodeClipQuota)
	}
	if storageErr.Requested != requested || storageErr.Used != used || storageErr.Limit != limit {
		t.Fatalf("unexpected error payload: %+v (want requested=%d used=%d limit=%d)",
			storageErr, requested, used, limit)
	}
	if storageErr.HTTPStatus() != 507 {
		t.Fatalf("clip quota refusals must render as 507, got %d", storageErr.HTTPStatus())
	}
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

// TestLegacyDatabaseMigratesClipQuota opens a database written before the
// saved-clip ledger existed and verifies ensureSchema creates clip_quota and
// seeds it with the bytes of the clips that are already stored (text clips
// carry a NULL file_size and therefore cost nothing).
func TestLegacyDatabaseMigratesClipQuota(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	statements := []string{
		`CREATE TABLE clips (
			id TEXT PRIMARY KEY, type TEXT NOT NULL, created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL, max_downloads INTEGER NOT NULL,
			download_count INTEGER NOT NULL, access_code TEXT UNIQUE,
			access_token TEXT, owner_id TEXT NOT NULL, text_content TEXT,
			file_name TEXT, file_path TEXT, file_size INTEGER, file_mime TEXT
		)`,
		`INSERT INTO clips VALUES ('old-1','file',1,9999999999,10,0,'code-1',NULL,'env-old',NULL,'a.bin','/tmp/a.bin',300,'application/octet-stream')`,
		`INSERT INTO clips VALUES ('old-2','text',1,9999999999,10,0,'code-2',NULL,'env-old','hello',NULL,NULL,NULL,NULL)`,
		`INSERT INTO clips VALUES ('old-3','file',1,9999999999,10,0,'code-3',NULL,'env-old',NULL,'b.bin','/tmp/b.bin',200,'application/octet-stream')`,
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

	var name string
	if err := repo.db.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='table' AND name='clip_quota'").Scan(&name); err != nil {
		t.Fatalf("clip_quota table missing after migration: %v", err)
	}
	if got := storedBytes(t, repo); got != 500 {
		t.Fatalf("seeded used_bytes = %d, want 500 (SUM(file_size) of the stored clips)", got)
	}
	// A restart must not re-seed / double count: the row already exists, so a
	// recompute is a no-op that still reports the seeded value.
	recomputed, previous, err := repo.RecomputeClipQuota()
	if err != nil || recomputed != 500 || previous != 500 {
		t.Fatalf("recompute after migration = %d (was %d, %v), want 500/500", recomputed, previous, err)
	}
}

// TestClipQuotaChargedOnInsertReturnedOnDelete is the core invariant: the
// ledger moves by exactly file_size on insert and back on delete; text clips
// are free and a foreign/absent delete must not touch the counter.
func TestClipQuotaChargedOnInsertReturnedOnDelete(t *testing.T) {
	repo := newTestRepository(t)

	if got := storedBytes(t, repo); got != 0 {
		t.Fatalf("fresh ledger = %d, want 0", got)
	}

	fileClip := createFileClip(t, repo, "a.bin", 400, "owner-a")
	if got := storedBytes(t, repo); got != 400 {
		t.Fatalf("used = %d, want 400 after one file clip", got)
	}

	createTextClip(t, repo, time.Now().Add(time.Hour), "owner-a", nil)
	if got := storedBytes(t, repo); got != 400 {
		t.Fatalf("text clips must not consume stored bytes: %d", got)
	}

	stored := createFileClip(t, repo, "b.bin", 600, "owner-b")
	if got := storedBytes(t, repo); got != 1000 {
		t.Fatalf("used = %d, want 1000 after two file clips", got)
	}
	if authoritative, err := repo.StoredClipBytes(); err != nil || authoritative != 1000 {
		t.Fatalf("SUM(file_size) = %d (%v), want 1000", authoritative, err)
	}

	// Wrong owner / unknown clip: no delete, no release.
	if removed, err := repo.DeleteClip(stored.ID, "someone-else"); err != nil || removed {
		t.Fatalf("foreign delete should fail: %v %v", removed, err)
	}
	if removed, err := repo.DeleteClip("missing-id", "owner-b"); err != nil || removed {
		t.Fatalf("unknown delete should fail: %v %v", removed, err)
	}
	if got := storedBytes(t, repo); got != 1000 {
		t.Fatalf("a refused delete must not move the ledger: %d", got)
	}

	if removed, err := repo.DeleteClip(stored.ID, "owner-b"); err != nil || !removed {
		t.Fatalf("delete should succeed: %v %v", removed, err)
	}
	if got := storedBytes(t, repo); got != 400 {
		t.Fatalf("used = %d, want 400 after deleting the 600 byte clip", got)
	}
	if removed, err := repo.DeleteClip(fileClip.ID, "owner-a"); err != nil || !removed {
		t.Fatalf("delete should succeed: %v %v", removed, err)
	}
	if got := storedBytes(t, repo); got != 0 {
		t.Fatalf("used = %d, want 0 after deleting everything", got)
	}
}

// TestClipQuotaRefusesInsertAtBoundary pins the CAS edge: a clip landing
// exactly on the budget is persisted, the next byte is refused, and a refused
// insert leaves neither a row nor a charge behind.
func TestClipQuotaRefusesInsertAtBoundary(t *testing.T) {
	repo := newTestRepository(t)
	const limit = 1000
	repo.Settings().StoredTotalQuotaBytes = limit

	first := createFileClip(t, repo, "first.bin", 400, "owner-q")
	createFileClip(t, repo, "second.bin", 600, "owner-q") // exactly the budget
	if got := storedBytes(t, repo); got != limit {
		t.Fatalf("used = %d, want %d", got, limit)
	}
	before := countClips(t, repo)

	_, err := repo.CreateClip(CreateClipParams{
		ClipType:      models.ClipTypeFile,
		ExpiresAtMs:   time.Now().Add(time.Hour).UnixMilli(),
		EnvironmentID: "owner-q",
		StoredFile:    fileValue("too-big.bin", 1),
	})
	assertClipQuotaError(t, err, 1, limit, limit)

	if got := storedBytes(t, repo); got != limit {
		t.Fatalf("a refused clip must not move the ledger: %d", got)
	}
	if got := countClips(t, repo); got != before {
		t.Fatalf("a refused clip must not be persisted: %d rows, want %d", got, before)
	}

	// Text clips are unaffected by a full file budget.
	createTextClip(t, repo, time.Now().Add(time.Hour), "owner-q", nil)

	// Freeing bytes makes the same insert possible: the budget is reusable.
	if removed, err := repo.DeleteClip(first.ID, "owner-q"); err != nil || !removed {
		t.Fatalf("delete should succeed: %v %v", removed, err)
	}
	if got := storedBytes(t, repo); got != 600 {
		t.Fatalf("used = %d, want 600 after freeing 400 bytes", got)
	}
	createFileClip(t, repo, "after-free.bin", 400, "owner-q")
	if got := storedBytes(t, repo); got != limit {
		t.Fatalf("used = %d, want %d after reusing the freed bytes", got, limit)
	}
}

// TestClipQuotaRefusalsCoverEveryFileClipEntryPoint: the chunked-upload COMPLETE
// path and the data-URL POST /api/clips path both funnel through CreateClip, so
// the refusal must not depend on which one called it - and a refused insert must
// leave the transaction (and therefore the ledger) untouched.
func TestClipQuotaRefusalsCoverEveryFileClipEntryPoint(t *testing.T) {
	repo := newTestRepository(t)
	repo.Settings().StoredTotalQuotaBytes = 100

	if _, err := repo.CreateClip(CreateClipParams{
		ClipType:      models.ClipTypeFile,
		ExpiresAtMs:   time.Now().Add(time.Hour).UnixMilli(),
		EnvironmentID: "owner-r",
		StoredFile:    fileValue("big.bin", 150),
	}); err == nil {
		t.Fatal("expected the first insert to be refused")
	} else {
		assertClipQuotaError(t, err, 150, 0, 100)
	}
	if got := storedBytes(t, repo); got != 0 {
		t.Fatalf("used = %d, want 0", got)
	}

	createFileClip(t, repo, "fits.bin", 100, "owner-r")

	// Duplicate access code + quota: the access-code conflict is detected first,
	// yet the ledger must stay at 100 either way.
	code := "dup-code"
	createTextClipWithCode(t, repo, code)
	_, err := repo.CreateClip(CreateClipParams{
		ClipType:      models.ClipTypeFile,
		ExpiresAtMs:   time.Now().Add(time.Hour).UnixMilli(),
		EnvironmentID: "owner-r",
		AccessCode:    &code,
		StoredFile:    fileValue("dup.bin", 10),
	})
	assertValueError(t, err, "直链码已存在，请刷新后再试")
	if got := storedBytes(t, repo); got != 100 {
		t.Fatalf("used = %d, want 100 after a rejected insert", got)
	}
}

func createTextClipWithCode(t *testing.T, repo *ClipRepository, code string) *models.Clip {
	t.Helper()
	clip, err := repo.CreateClip(CreateClipParams{
		ClipType:      models.ClipTypeText,
		ExpiresAtMs:   time.Now().Add(time.Hour).UnixMilli(),
		EnvironmentID: "owner-r",
		AccessCode:    &code,
		Text:          textValue("payload"),
	})
	if err != nil {
		t.Fatalf("unable to create coded text clip: %v", err)
	}
	return clip
}

// TestClipQuotaUnlimitedStillTracks: limit 0 disables the refusal but keeps the
// counter honest, so an operator can switch the budget on later without a
// recompute.
func TestClipQuotaUnlimitedStillTracks(t *testing.T) {
	repo := newTestRepository(t)
	repo.Settings().StoredTotalQuotaBytes = 0

	createFileClip(t, repo, "huge.bin", 1<<40, "owner-u")
	if got := storedBytes(t, repo); got != 1<<40 {
		t.Fatalf("used = %d, want %d with the quota disabled", got, int64(1<<40))
	}
}

// TestClipQuotaReleasedOnPurgeInactive covers the periodic cleanup: expired and
// download-exhausted clips are deleted by PurgeInactive (startup purge, cleanup
// worker and the list endpoint all call it), and their bytes must come back.
func TestClipQuotaReleasedOnPurgeInactive(t *testing.T) {
	repo := newTestRepository(t)

	expired := createFileClip(t, repo, "expired.bin", 700, "owner-p")
	if _, err := repo.db.Exec("UPDATE clips SET expires_at = ? WHERE id = ?",
		time.Now().Add(-time.Minute).Unix(), expired.ID); err != nil {
		t.Fatalf("unable to expire clip: %v", err)
	}

	one := 1
	exhaustedText := createTextClip(t, repo, time.Now().Add(time.Hour), "owner-p", &one)
	if _, _, err := repo.IncrementDownloads(exhaustedText.ID, "owner-p"); err != nil {
		t.Fatalf("unable to exhaust downloads: %v", err)
	}
	exhaustedFile, err := repo.CreateClip(CreateClipParams{
		ClipType:      models.ClipTypeFile,
		ExpiresAtMs:   time.Now().Add(time.Hour).UnixMilli(),
		EnvironmentID: "owner-p",
		MaxDownloads:  &one,
		StoredFile:    fileValue("downloaded.bin", 250),
	})
	if err != nil {
		t.Fatalf("unable to create exhausted file clip: %v", err)
	}
	if _, _, err := repo.IncrementDownloads(exhaustedFile.ID, "owner-p"); err != nil {
		t.Fatalf("unable to exhaust downloads: %v", err)
	}

	alive := createFileClip(t, repo, "alive.bin", 50, "owner-p")
	if got := storedBytes(t, repo); got != 1000 {
		t.Fatalf("used = %d, want 1000 (700 + 250 + 50)", got)
	}

	purged, err := repo.PurgeInactive()
	if err != nil {
		t.Fatalf("purge inactive: %v", err)
	}
	if purged != 3 {
		t.Fatalf("expected 3 purged clips, got %d", purged)
	}
	if got := storedBytes(t, repo); got != 50 {
		t.Fatalf("used = %d, want 50 (only the live clip survives)", got)
	}
	if authoritative, err := repo.StoredClipBytes(); err != nil || authoritative != 50 {
		t.Fatalf("SUM(file_size) = %d (%v), want 50", authoritative, err)
	}

	// Purging again must not double-release.
	if _, err := repo.PurgeInactive(); err != nil {
		t.Fatalf("second purge: %v", err)
	}
	if got := storedBytes(t, repo); got != 50 {
		t.Fatalf("used = %d, want 50 after a no-op purge", got)
	}
	if clip, err := repo.GetClip(alive.ID); err != nil || clip == nil {
		t.Fatalf("live clip must survive: %v %v", clip, err)
	}
}

// TestClipQuotaConcurrentInsertsNoOversell proves the CAS: N parallel inserts
// against a budget that fits M of them let exactly M through, and the ledger
// ends up exactly on the budget.
func TestClipQuotaConcurrentInsertsNoOversell(t *testing.T) {
	repo := newTestRepository(t)
	const (
		limit     = 1000
		perClip   = 100
		attempts  = 40
		expectedN = limit / perClip
	)
	repo.Settings().StoredTotalQuotaBytes = limit

	var accepted, refused, otherErrors int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			_, err := repo.CreateClip(CreateClipParams{
				ClipType:      models.ClipTypeFile,
				ExpiresAtMs:   time.Now().Add(time.Hour).UnixMilli(),
				EnvironmentID: "owner-race",
				StoredFile:    fileValue("race.bin", perClip),
			})
			var storageErr *apperr.StorageError
			switch {
			case err == nil:
				atomic.AddInt64(&accepted, 1)
			case errors.As(err, &storageErr) && storageErr.Code == apperr.StorageCodeClipQuota:
				atomic.AddInt64(&refused, 1)
			default:
				atomic.AddInt64(&otherErrors, 1)
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if otherErrors != 0 {
		t.Fatalf("unexpected failures: %d", otherErrors)
	}
	if accepted != expectedN || refused != attempts-expectedN {
		t.Fatalf("accepted = %d, refused = %d; want %d / %d", accepted, refused, expectedN, attempts-expectedN)
	}
	if got := storedBytes(t, repo); got != limit {
		t.Fatalf("used = %d, want exactly the budget %d", got, limit)
	}
	if got := countClips(t, repo); got != expectedN {
		t.Fatalf("stored clips = %d, want %d", got, expectedN)
	}
}

// TestClipQuotaRecomputeRepairsDrift covers the reconciliation the cleanup
// worker runs every tick: the ledger is overwritten with the authoritative
// SUM(file_size) and the budget becomes usable again.
func TestClipQuotaRecomputeRepairsDrift(t *testing.T) {
	repo := newTestRepository(t)
	repo.Settings().StoredTotalQuotaBytes = 1000

	createFileClip(t, repo, "kept.bin", 400, "owner-drift")
	if err := repo.ForceClipQuotaForTest(99999); err != nil {
		t.Fatalf("force drift: %v", err)
	}
	// Drifted high: the store looks full.
	_, err := repo.CreateClip(CreateClipParams{
		ClipType:      models.ClipTypeFile,
		ExpiresAtMs:   time.Now().Add(time.Hour).UnixMilli(),
		EnvironmentID: "owner-drift",
		StoredFile:    fileValue("blocked.bin", 10),
	})
	assertClipQuotaError(t, err, 10, 99999, 1000)

	recomputed, previous, err := repo.RecomputeClipQuota()
	if err != nil {
		t.Fatalf("recompute: %v", err)
	}
	if recomputed != 400 || previous != 99999 {
		t.Fatalf("recompute = %d (was %d), want 400 (was 99999)", recomputed, previous)
	}
	if got := storedBytes(t, repo); got != 400 {
		t.Fatalf("used = %d, want 400 after the repair", got)
	}
	createFileClip(t, repo, "unblocked.bin", 600, "owner-drift")

	// Drifted low: the repair must also correct an impossible undercount.
	if err := repo.ForceClipQuotaForTest(7); err != nil {
		t.Fatalf("force drift: %v", err)
	}
	recomputed, previous, err = repo.RecomputeClipQuota()
	if err != nil {
		t.Fatalf("recompute: %v", err)
	}
	if recomputed != 1000 || previous != 7 {
		t.Fatalf("recompute = %d (was %d), want 1000 (was 7)", recomputed, previous)
	}
	// A second run is a no-op (no drift).
	recomputed, previous, err = repo.RecomputeClipQuota()
	if err != nil || recomputed != previous || recomputed != 1000 {
		t.Fatalf("idempotent recompute = %d/%d (%v), want 1000/1000", recomputed, previous, err)
	}
}

// TestClipQuotaMissingLedgerRowSelfHeals: a hand-deleted ledger row must not
// break clip creation. The next charge re-seeds the row from the clips table,
// which is the convergent value.
func TestClipQuotaMissingLedgerRowSelfHeals(t *testing.T) {
	repo := newTestRepository(t)
	repo.Settings().StoredTotalQuotaBytes = 1000

	createFileClip(t, repo, "before.bin", 300, "owner-heal")
	if _, err := repo.db.Exec("DELETE FROM clip_quota WHERE id = 1"); err != nil {
		t.Fatalf("unable to drop the ledger row: %v", err)
	}
	if _, err := repo.GetClipQuota(); err == nil {
		t.Fatal("expected the ledger row to be missing")
	}

	createFileClip(t, repo, "after.bin", 200, "owner-heal")
	if got := storedBytes(t, repo); got != 500 {
		t.Fatalf("used = %d, want 500 (re-seeded 300 + 200)", got)
	}

	// Deleting a clip while the row is missing must not resurrect a negative
	// counter either: releasing is a no-op, and the next charge re-seeds from
	// the clips table (which already accounts for the deleted row).
	second := createFileClip(t, repo, "second.bin", 0, "owner-heal")
	if _, err := repo.db.Exec("DELETE FROM clip_quota WHERE id = 1"); err != nil {
		t.Fatalf("unable to drop the ledger row: %v", err)
	}
	if removed, err := repo.DeleteClip(second.ID, "owner-heal"); err != nil || !removed {
		t.Fatalf("delete should succeed: %v %v", removed, err)
	}
	createFileClip(t, repo, "healed.bin", 100, "owner-heal")

	used := storedBytes(t, repo)
	authoritative, err := repo.StoredClipBytes()
	if err != nil {
		t.Fatalf("sum clips: %v", err)
	}
	// 300 (before.bin) + 200 (after.bin) + 0 (second.bin, deleted while the row
	// was missing) + 100 (healed.bin).
	if used != authoritative || used != 600 {
		t.Fatalf("used = %d, SUM(file_size) = %d; want 600/600 (re-seeded after the manual delete)",
			used, authoritative)
	}
}
