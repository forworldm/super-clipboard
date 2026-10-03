package repository

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pixia1234/super-clipboard/backend/internal/apperr"
	"github.com/pixia1234/super-clipboard/backend/internal/config"
	"github.com/pixia1234/super-clipboard/backend/internal/models"
)

func newTestRepository(t *testing.T) *ClipRepository {
	t.Helper()
	dir := t.TempDir()
	settings := config.Defaults()
	settings.DatabasePath = filepath.Join(dir, "clips.db")
	settings.FileStorageDir = filepath.Join(dir, "files")
	repo, err := NewClipRepository(settings)
	if err != nil {
		t.Fatalf("unable to open repository: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

func textValue(value string) *string { return &value }

func createTextClip(t *testing.T, repo *ClipRepository, expiresAt time.Time, owner string, maxDownloads *int) *models.Clip {
	t.Helper()
	clip, err := repo.CreateClip(CreateClipParams{
		ClipType:      models.ClipTypeText,
		ExpiresAtMs:   expiresAt.UnixMilli(),
		MaxDownloads:  maxDownloads,
		EnvironmentID: owner,
		Text:          textValue("payload"),
	})
	if err != nil {
		t.Fatalf("unable to create clip: %v", err)
	}
	return clip
}

// TestSchemaIsCreated covers _ensure_schema, including the owner_id migration.
func TestSchemaIsCreated(t *testing.T) {
	repo := newTestRepository(t)

	for _, table := range []string{"clips", "tokens"} {
		var name string
		if err := repo.db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name); err != nil {
			t.Fatalf("expected table %s: %v", table, err)
		}
	}

	rows, err := repo.db.Query("PRAGMA table_info(clips)")
	if err != nil {
		t.Fatalf("unable to inspect clips: %v", err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var (
			cid, notNull, pk int
			name, declType   string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &declType, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("unable to read column info: %v", err)
		}
		columns[name] = true
	}
	for _, expected := range []string{"id", "type", "created_at", "expires_at", "max_downloads",
		"download_count", "access_code", "access_token", "owner_id", "text_content",
		"file_name", "file_path", "file_size", "file_mime"} {
		if !columns[expected] {
			t.Fatalf("missing column %s (have %v)", expected, columns)
		}
	}
}

// TestSanitizeMaxDownloads covers sanitize_max_downloads.
func TestSanitizeMaxDownloads(t *testing.T) {
	repo := newTestRepository(t)

	if got := repo.SanitizeMaxDownloads(nil); got != repo.Settings().DefaultMaxDownloads {
		t.Fatalf("expected default %d, got %d", repo.Settings().DefaultMaxDownloads, got)
	}
	zero := 0
	if got := repo.SanitizeMaxDownloads(&zero); got != 1 {
		t.Fatalf("expected clamp to 1, got %d", got)
	}
	huge := 100000
	if got := repo.SanitizeMaxDownloads(&huge); got != repo.Settings().MaxAllowedDownloads {
		t.Fatalf("expected clamp to %d, got %d", repo.Settings().MaxAllowedDownloads, got)
	}
	normal := 7
	if got := repo.SanitizeMaxDownloads(&normal); got != 7 {
		t.Fatalf("expected 7, got %d", got)
	}
}

// TestCreateClipValidation covers the ValueError paths of create_clip.
func TestCreateClipValidation(t *testing.T) {
	repo := newTestRepository(t)

	_, err := repo.CreateClip(CreateClipParams{
		ClipType:      models.ClipTypeText,
		ExpiresAtMs:   time.Now().Add(-time.Hour).UnixMilli(),
		EnvironmentID: "owner",
		Text:          textValue("payload"),
	})
	assertValueError(t, err, "过期时间必须晚于当前时间")

	_, err = repo.CreateClip(CreateClipParams{
		ClipType:      models.ClipTypeText,
		ExpiresAtMs:   time.Now().Add(time.Hour).UnixMilli(),
		EnvironmentID: "   ",
		Text:          textValue("payload"),
	})
	assertValueError(t, err, "剪贴板所属标识缺失")

	code := "12345"
	if _, err := repo.CreateClip(CreateClipParams{
		ClipType:      models.ClipTypeText,
		ExpiresAtMs:   time.Now().Add(time.Hour).UnixMilli(),
		AccessCode:    &code,
		EnvironmentID: "owner",
		Text:          textValue("payload"),
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, err = repo.CreateClip(CreateClipParams{
		ClipType:      models.ClipTypeText,
		ExpiresAtMs:   time.Now().Add(time.Hour).UnixMilli(),
		AccessCode:    &code,
		EnvironmentID: "owner",
		Text:          textValue("payload"),
	})
	assertValueError(t, err, "直链码已存在，请刷新后再试")
}

func assertValueError(t *testing.T, err error, expected string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %q, got nil", expected)
	}
	valueError, ok := err.(*apperr.ValueError)
	if !ok {
		t.Fatalf("expected a ValueError, got %T (%v)", err, err)
	}
	if !strings.Contains(valueError.Message, expected) {
		t.Fatalf("expected %q, got %q", expected, valueError.Message)
	}
}

// TestListAndGetters covers list_clips and the lookup helpers.
func TestListAndGetters(t *testing.T) {
	repo := newTestRepository(t)
	code := "31415"
	token := "token-abc"
	clip, err := repo.CreateClip(CreateClipParams{
		ClipType:      models.ClipTypeText,
		ExpiresAtMs:   time.Now().Add(time.Hour).UnixMilli(),
		MaxDownloads:  nil,
		AccessCode:    &code,
		AccessToken:   &token,
		EnvironmentID: "owner-a",
		Text:          textValue("hello"),
	})
	if err != nil {
		t.Fatalf("unable to create clip: %v", err)
	}
	if _, err := repo.CreateClip(CreateClipParams{
		ClipType:      models.ClipTypeText,
		ExpiresAtMs:   time.Now().Add(time.Hour).UnixMilli(),
		EnvironmentID: "owner-b",
		Text:          textValue("other"),
	}); err != nil {
		t.Fatalf("unable to create second clip: %v", err)
	}

	listed, err := repo.ListClips("owner-a")
	if err != nil {
		t.Fatalf("unable to list clips: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != clip.ID {
		t.Fatalf("unexpected list result %v", listed)
	}
	if listed[0].AccessCode == nil || *listed[0].AccessCode != code {
		t.Fatalf("access code not persisted: %v", listed[0].AccessCode)
	}
	if listed[0].Text == nil || *listed[0].Text != "hello" {
		t.Fatalf("text not persisted: %v", listed[0].Text)
	}
	if listed[0].MaxDownloads != repo.Settings().DefaultMaxDownloads {
		t.Fatalf("expected default max downloads, got %d", listed[0].MaxDownloads)
	}

	byID, err := repo.GetClip(clip.ID)
	if err != nil || byID == nil {
		t.Fatalf("unable to fetch clip by id: %v %v", byID, err)
	}
	byCode, err := repo.GetClipByCode(code)
	if err != nil || byCode == nil || byCode.ID != clip.ID {
		t.Fatalf("unable to fetch clip by code: %v %v", byCode, err)
	}
	byCodeAndOwner, err := repo.GetClipByCodeAndOwner(code, "owner-a")
	if err != nil || byCodeAndOwner == nil {
		t.Fatalf("unable to fetch clip by code and owner: %v %v", byCodeAndOwner, err)
	}
	if other, err := repo.GetClipByCodeAndOwner(code, "owner-b"); err != nil || other != nil {
		t.Fatalf("foreign owner should not match: %v %v", other, err)
	}
	byToken, err := repo.GetClipByToken(token, "")
	if err != nil || byToken == nil || byToken.ID != clip.ID {
		t.Fatalf("unable to fetch clip by token: %v %v", byToken, err)
	}
	scoped, err := repo.GetClipByToken(token, "owner-a")
	if err != nil || scoped == nil {
		t.Fatalf("unable to fetch scoped clip by token: %v %v", scoped, err)
	}
	if missing, err := repo.GetClipByToken(token, "owner-b"); err != nil || missing != nil {
		t.Fatalf("scoped lookup should miss: %v %v", missing, err)
	}
	if missing, err := repo.GetClip("nope"); err != nil || missing != nil {
		t.Fatalf("unknown id should miss: %v %v", missing, err)
	}
}

// TestDeleteClip covers delete_clip including the stored file cleanup.
func TestDeleteClip(t *testing.T) {
	repo := newTestRepository(t)
	storedPath := filepath.Join(repo.Settings().FileStorageDir, "stored.bin")
	if err := os.MkdirAll(repo.Settings().FileStorageDir, 0o755); err != nil {
		t.Fatalf("unable to prepare storage dir: %v", err)
	}
	if err := os.WriteFile(storedPath, []byte("content"), 0o644); err != nil {
		t.Fatalf("unable to write stored file: %v", err)
	}

	clip, err := repo.CreateClip(CreateClipParams{
		ClipType:      models.ClipTypeFile,
		ExpiresAtMs:   time.Now().Add(time.Hour).UnixMilli(),
		EnvironmentID: "owner-del",
		StoredFile:    &models.StoredFile{Name: "stored.bin", Size: 7, Mime: "application/octet-stream", Path: storedPath},
	})
	if err != nil {
		t.Fatalf("unable to create clip: %v", err)
	}
	if clip.StoredFile == nil || clip.StoredFile.Name != "stored.bin" {
		t.Fatalf("stored file not persisted: %v", clip.StoredFile)
	}

	if removed, err := repo.DeleteClip(clip.ID, "someone-else"); err != nil || removed {
		t.Fatalf("foreign delete should fail: %v %v", removed, err)
	}
	if removed, err := repo.DeleteClip(clip.ID, "   "); err != nil || removed {
		t.Fatalf("blank owner should fail: %v %v", removed, err)
	}
	if removed, err := repo.DeleteClip(clip.ID, "owner-del"); err != nil || !removed {
		t.Fatalf("delete should succeed: %v %v", removed, err)
	}
	if _, err := os.Stat(storedPath); !os.IsNotExist(err) {
		t.Fatalf("stored file should be removed, got %v", err)
	}
	if clip, err := repo.GetClip(clip.ID); err != nil || clip != nil {
		t.Fatalf("clip should be gone: %v %v", clip, err)
	}
}

// TestIncrementDownloads covers increment_downloads and its ownership guard.
func TestIncrementDownloads(t *testing.T) {
	repo := newTestRepository(t)
	maxDownloads := 2
	clip := createTextClip(t, repo, time.Now().Add(time.Hour), "owner-inc", &maxDownloads)

	updated, reached, err := repo.IncrementDownloads(clip.ID, "owner-inc")
	if err != nil || updated == nil || reached {
		t.Fatalf("unexpected first increment: %v %v %v", updated, reached, err)
	}
	if updated.DownloadCount != 1 {
		t.Fatalf("expected download count 1, got %d", updated.DownloadCount)
	}
	if updated.Text == nil || *updated.Text != "payload" {
		t.Fatalf("payload should survive the update: %v", updated.Text)
	}

	updated, reached, err = repo.IncrementDownloads(clip.ID, "owner-inc")
	if err != nil || updated == nil || !reached {
		t.Fatalf("unexpected second increment: %v %v %v", updated, reached, err)
	}

	if updated, reached, err := repo.IncrementDownloads(clip.ID, "intruder"); err != nil || updated != nil || reached {
		t.Fatalf("foreign increment should be rejected: %v %v %v", updated, reached, err)
	}
	if updated, reached, err := repo.IncrementDownloads("missing", "owner-inc"); err != nil || updated != nil || reached {
		t.Fatalf("unknown clip should be rejected: %v %v %v", updated, reached, err)
	}
	if updated, reached, err := repo.IncrementDownloads(clip.ID, ""); err != nil || updated != nil || reached {
		t.Fatalf("blank owner should be rejected: %v %v %v", updated, reached, err)
	}
}

// TestPurgeInactive covers purge_inactive for expired and exhausted clips.
func TestPurgeInactive(t *testing.T) {
	repo := newTestRepository(t)

	expired := createTextClip(t, repo, time.Now().Add(time.Hour), "owner-purge", nil)
	if _, err := repo.db.Exec("UPDATE clips SET expires_at = ? WHERE id = ?", time.Now().Add(-time.Minute).Unix(), expired.ID); err != nil {
		t.Fatalf("unable to expire clip: %v", err)
	}

	one := 1
	exhausted := createTextClip(t, repo, time.Now().Add(time.Hour), "owner-purge", &one)
	if _, _, err := repo.IncrementDownloads(exhausted.ID, "owner-purge"); err != nil {
		t.Fatalf("unable to increment downloads: %v", err)
	}

	if err := os.MkdirAll(repo.Settings().FileStorageDir, 0o755); err != nil {
		t.Fatalf("unable to prepare storage dir: %v", err)
	}
	stalePath := filepath.Join(repo.Settings().FileStorageDir, "stale.bin")
	if err := os.WriteFile(stalePath, []byte("stale"), 0o644); err != nil {
		t.Fatalf("unable to write stale file: %v", err)
	}
	stale, err := repo.CreateClip(CreateClipParams{
		ClipType:      models.ClipTypeFile,
		ExpiresAtMs:   time.Now().Add(time.Hour).UnixMilli(),
		EnvironmentID: "owner-purge",
		StoredFile:    &models.StoredFile{Name: "stale.bin", Size: 5, Mime: "application/octet-stream", Path: stalePath},
	})
	if err != nil {
		t.Fatalf("stale clip should still be creatable: %v", err)
	}
	if _, err := repo.db.Exec("UPDATE clips SET expires_at = ? WHERE id = ?", time.Now().Add(-time.Minute).Unix(), stale.ID); err != nil {
		t.Fatalf("unable to expire stale clip: %v", err)
	}

	alive := createTextClip(t, repo, time.Now().Add(time.Hour), "owner-purge", nil)

	purged, err := repo.PurgeInactive()
	if err != nil {
		t.Fatalf("unable to purge: %v", err)
	}
	if purged != 3 {
		t.Fatalf("expected 3 removed clips, got %d", purged)
	}
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Fatalf("stored file of a purged clip should be removed: %v", err)
	}
	if clip, err := repo.GetClip(alive.ID); err != nil || clip == nil {
		t.Fatalf("active clip should survive the purge: %v %v", clip, err)
	}
}

// TestTokenLifecycle covers register_token / ensure_token_owner.
func TestTokenLifecycle(t *testing.T) {
	repo := newTestRepository(t)

	record, err := repo.RegisterToken("  token-lifecycle  ", nil)
	if err != nil {
		t.Fatalf("unable to register token: %v", err)
	}
	if record.Token != "token-lifecycle" {
		t.Fatalf("token should be trimmed, got %q", record.Token)
	}
	if record.EnvironmentID == "" {
		t.Fatalf("expected a generated owner")
	}
	if record.LastUsedAt != nil {
		t.Fatalf("expected nil lastUsedAt, got %v", *record.LastUsedAt)
	}
	if record.ExpiresAt <= record.UpdatedAt {
		t.Fatalf("expected a future expiry: %v", record)
	}

	// Same owner refreshes the token.
	refreshed, err := repo.RegisterToken("token-lifecycle", &record.EnvironmentID)
	if err != nil {
		t.Fatalf("same owner should be able to re-register: %v", err)
	}
	if refreshed.EnvironmentID != record.EnvironmentID {
		t.Fatalf("owner changed: %s vs %s", refreshed.EnvironmentID, record.EnvironmentID)
	}

	// Another device is rejected.
	other := "other-device"
	if _, err := repo.RegisterToken("token-lifecycle", &other); err == nil {
		t.Fatalf("expected a conflict")
	} else {
		assertValueError(t, err, "持久 Token 已被其他设备占用，请稍后重试")
	}

	// Blank tokens are rejected.
	if _, err := repo.RegisterToken("   ", nil); err == nil {
		t.Fatalf("expected an error for a blank token")
	} else {
		assertValueError(t, err, "持久 Token 无效")
	}

	// ensure_token_owner updates last_used_at.
	ensured, err := repo.EnsureTokenOwner("token-lifecycle", record.EnvironmentID)
	if err != nil {
		t.Fatalf("owner check failed: %v", err)
	}
	if ensured.LastUsedAt == nil {
		t.Fatalf("expected lastUsedAt to be set")
	}
	if ensured.ExpiresAt < ensured.UpdatedAt {
		t.Fatalf("expected a refreshed expiry: %v", ensured)
	}

	if _, err := repo.EnsureTokenOwner("token-lifecycle", "intruder"); err == nil {
		t.Fatalf("expected an ownership conflict")
	} else {
		assertValueError(t, err, "持久 Token 已被其他设备占用，请稍后重试")
	}
	if _, err := repo.EnsureTokenOwner("unknown-token", record.EnvironmentID); err == nil {
		t.Fatalf("expected an unknown token error")
	} else {
		assertValueError(t, err, "持久 Token 未注册，请重新保存")
	}
	if _, err := repo.EnsureTokenOwner("token-lifecycle", "  "); err == nil {
		t.Fatalf("expected a validation error")
	} else {
		assertValueError(t, err, "Token 校验失败")
	}

	// An expired token is dropped and reported as expired.
	if _, err := repo.db.Exec("UPDATE tokens SET expires_at = ? WHERE token = ?", time.Now().Add(-time.Minute).Unix(), "token-lifecycle"); err != nil {
		t.Fatalf("unable to expire token: %v", err)
	}
	if _, err := repo.EnsureTokenOwner("token-lifecycle", record.EnvironmentID); err == nil {
		t.Fatalf("expected an expired token error")
	} else {
		assertValueError(t, err, "持久 Token 已过期，请重新生成")
	}
	var remaining int
	if err := repo.db.QueryRow("SELECT COUNT(*) FROM tokens WHERE token = ?", "token-lifecycle").Scan(&remaining); err != nil {
		t.Fatalf("unable to count tokens: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("expired token should be deleted, found %d", remaining)
	}

	// Re-registering an expired row reassigns the owner.
	if _, err := repo.db.Exec(
		"INSERT INTO tokens (token, owner_id, updated_at, last_used_at, expires_at) VALUES (?, ?, ?, NULL, ?)",
		"stale-token", "stale-owner", time.Now().Add(-2*time.Hour).Unix(), time.Now().Add(-time.Hour).Unix(),
	); err != nil {
		t.Fatalf("unable to seed stale token: %v", err)
	}
	reassigned, err := repo.RegisterToken("stale-token", &record.EnvironmentID)
	if err != nil {
		t.Fatalf("unable to re-register stale token: %v", err)
	}
	if reassigned.EnvironmentID == "stale-owner" {
		t.Fatalf("stale owner should be replaced, got %s", reassigned.EnvironmentID)
	}
	if reassigned.LastUsedAt != nil {
		t.Fatalf("lastUsedAt should be reset")
	}
}

// TestConcurrentIncrements makes sure the write lock keeps the counter exact.
func TestConcurrentIncrements(t *testing.T) {
	repo := newTestRepository(t)
	maxDownloads := 500
	clip := createTextClip(t, repo, time.Now().Add(time.Hour), "owner-concurrent", &maxDownloads)

	const workers = 16
	done := make(chan bool, workers)
	for index := 0; index < workers; index++ {
		go func() {
			_, _, _ = repo.IncrementDownloads(clip.ID, "owner-concurrent")
			done <- true
		}()
	}
	for index := 0; index < workers; index++ {
		<-done
	}

	updated, err := repo.GetClip(clip.ID)
	if err != nil || updated == nil {
		t.Fatalf("unable to reload clip: %v", err)
	}
	if updated.DownloadCount != workers {
		t.Fatalf("expected %d downloads, got %d", workers, updated.DownloadCount)
	}
}
