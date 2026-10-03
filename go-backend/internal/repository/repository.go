// Package repository is the Go port of backend/repository.py.
//
// The SQLite schema, the on-disk layout and every user facing error message are
// kept identical to the Python implementation so that a database produced by
// the FastAPI backend can be reused by the Go backend (and vice versa).
package repository

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/pixia1234/super-clipboard/backend/internal/apperr"
	"github.com/pixia1234/super-clipboard/backend/internal/config"
	"github.com/pixia1234/super-clipboard/backend/internal/models"
)

// createTableStatements mirrors CREATE_TABLE_SQL from repository.py.
// The upload_* tables are new (chunked upload sessions) and are additive:
// existing clips/tokens databases migrate automatically via IF NOT EXISTS.
var createTableStatements = []string{
	`CREATE TABLE IF NOT EXISTS clips (
		id TEXT PRIMARY KEY,
		type TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL,
		max_downloads INTEGER NOT NULL,
		download_count INTEGER NOT NULL,
		access_code TEXT UNIQUE,
		access_token TEXT,
		owner_id TEXT NOT NULL,
		text_content TEXT,
		file_name TEXT,
		file_path TEXT,
		file_size INTEGER,
		file_mime TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS idx_clips_expires_at ON clips(expires_at)`,
	`CREATE INDEX IF NOT EXISTS idx_clips_access_code ON clips(access_code)`,
	`CREATE INDEX IF NOT EXISTS idx_clips_access_token ON clips(access_token)`,
	`CREATE TABLE IF NOT EXISTS tokens (
		token TEXT PRIMARY KEY,
		owner_id TEXT NOT NULL,
		updated_at INTEGER NOT NULL,
		last_used_at INTEGER,
		expires_at INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_tokens_expires_at ON tokens(expires_at)`,
	`CREATE TABLE IF NOT EXISTS upload_sessions (
		id TEXT PRIMARY KEY,
		filename TEXT NOT NULL,
		file_size INTEGER NOT NULL,
		mime_type TEXT NOT NULL,
		chunk_size INTEGER NOT NULL,
		total_chunks INTEGER NOT NULL,
		status TEXT NOT NULL,
		environment_id TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL,
		staged_path TEXT,
		clip_id TEXT,
		request_id TEXT,
		quota_released INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS idx_upload_sessions_expires_at ON upload_sessions(expires_at)`,
	`CREATE INDEX IF NOT EXISTS idx_upload_sessions_status ON upload_sessions(status)`,
	`CREATE TABLE IF NOT EXISTS upload_chunks (
		upload_id TEXT NOT NULL,
		chunk_index INTEGER NOT NULL,
		size INTEGER NOT NULL,
		received_at INTEGER NOT NULL,
		PRIMARY KEY (upload_id, chunk_index)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_upload_chunks_upload_id ON upload_chunks(upload_id)`,
	// upload_quota is a single-row ledger (id=1) holding the bytes currently
	// reserved by live upload sessions. Reservations are moved in/out with
	// atomic UPDATE ... WHERE predicates, never read-modify-write.
	`CREATE TABLE IF NOT EXISTS upload_quota (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		reserved_bytes INTEGER NOT NULL DEFAULT 0,
		updated_at INTEGER NOT NULL DEFAULT 0
	)`,
}

// uploadQuotaSeedRow creates the single ledger row for databases that predate
// the quota feature. The starting balance is the reservation the existing
// active sessions would hold, so an upgrade never grants free credit.
const uploadQuotaSeedRow = `INSERT INTO upload_quota (id, reserved_bytes, updated_at)
	SELECT 1, COALESCE(SUM(file_size), 0), strftime('%s','now')
	FROM upload_sessions WHERE status = 'active' AND quota_released = 0
	ON CONFLICT(id) DO NOTHING`

// uploadRequestIDIndex enforces init idempotency on (environment_id,
// request_id) for sessions that carry a client requestId. Partial index so
// legacy rows without a key never conflict (SQLite partial indexes supported).
const uploadRequestIDIndex = `CREATE UNIQUE INDEX IF NOT EXISTS idx_upload_sessions_env_request
	ON upload_sessions(environment_id, request_id) WHERE request_id IS NOT NULL`

// clipColumns keeps SELECT * behaviour stable across schema migrations.
const clipColumns = `id, type, created_at, expires_at, max_downloads, download_count,
	access_code, access_token, owner_id, text_content, file_name, file_path, file_size, file_mime`

// TokenRecord mirrors the dict returned by register_token/ensure_token_owner.
type TokenRecord struct {
	Token         string
	EnvironmentID string
	UpdatedAt     int64
	LastUsedAt    *int64
	ExpiresAt     int64
}

// ClipRepository mirrors the ClipRepository class. The mutex reproduces the
// threading.Lock that guards every write in the Python implementation.
type ClipRepository struct {
	settings *config.Settings
	db       *sql.DB
	mu       sync.Mutex
}

// NewClipRepository opens (and migrates) the SQLite database.
func NewClipRepository(settings *config.Settings) (*ClipRepository, error) {
	if err := os.MkdirAll(filepath.Dir(settings.DatabasePath), 0o755); err != nil {
		return nil, fmt.Errorf("unable to create database directory: %w", err)
	}
	db, err := openDatabase(settings.DatabasePath)
	if err != nil {
		return nil, err
	}
	repo := &ClipRepository{settings: settings, db: db}
	if err := repo.ensureSchema(); err != nil {
		db.Close()
		return nil, err
	}
	return repo, nil
}

// Close releases the database handle.
func (r *ClipRepository) Close() error {
	if r.db == nil {
		return nil
	}
	return r.db.Close()
}

// Settings exposes the runtime configuration (default/max downloads, token TTL).
func (r *ClipRepository) Settings() *config.Settings { return r.settings }

func openDatabase(path string) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		absolute = path
	}
	dsn := buildDSN(absolute)
	db, err := sql.Open("sqlite", dsn)
	if err == nil {
		db.SetMaxOpenConns(8)
		db.SetMaxIdleConns(4)
		db.SetConnMaxLifetime(time.Hour)
		if pingErr := db.Ping(); pingErr == nil {
			return db, nil
		} else {
			db.Close()
			err = pingErr
		}
	}
	// Fallback for exotic paths: plain filename with a single serialized
	// connection, which still provides the same guarantees.
	db, openErr := sql.Open("sqlite", absolute)
	if openErr != nil {
		return nil, fmt.Errorf("unable to open sqlite database: %w", errors.Join(err, openErr))
	}
	db.SetMaxOpenConns(1)
	if pingErr := db.Ping(); pingErr != nil {
		db.Close()
		return nil, fmt.Errorf("unable to open sqlite database: %w", errors.Join(err, pingErr))
	}
	if _, pragmaErr := db.Exec("PRAGMA busy_timeout=10000"); pragmaErr != nil {
		db.Close()
		return nil, pragmaErr
	}
	return db, nil
}

func buildDSN(absolutePath string) string {
	escaped := strings.NewReplacer(
		"%", "%25",
		"?", "%3f",
		"#", "%23",
		" ", "%20",
	).Replace(absolutePath)
	return "file:" + escaped +
		"?_pragma=busy_timeout(10000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)"
}

func (r *ClipRepository) ensureSchema() error {
	for _, statement := range createTableStatements {
		if _, err := r.db.Exec(statement); err != nil {
			return fmt.Errorf("unable to initialize schema: %w", err)
		}
	}
	columns, err := r.tableColumns("clips")
	if err != nil {
		return err
	}
	if !columns["owner_id"] {
		if _, err := r.db.Exec("ALTER TABLE clips ADD COLUMN owner_id TEXT DEFAULT ''"); err != nil {
			return fmt.Errorf("unable to migrate schema: %w", err)
		}
		if _, err := r.db.Exec("UPDATE clips SET owner_id = '' WHERE owner_id IS NULL"); err != nil {
			return fmt.Errorf("unable to migrate schema: %w", err)
		}
	}
	// upload_sessions.request_id: add column for databases created before
	// chunked-upload idempotency existed, then build the partial UNIQUE index.
	uploadColumns, err := r.tableColumns("upload_sessions")
	if err != nil {
		return err
	}
	if !uploadColumns["request_id"] {
		if _, err := r.db.Exec("ALTER TABLE upload_sessions ADD COLUMN request_id TEXT"); err != nil {
			return fmt.Errorf("unable to migrate schema: %w", err)
		}
		// De-dup guard: legacy rows carry NULL, which the partial index ignores.
	}
	// upload_sessions.quota_released: 0 = this session still holds its upload
	// quota reservation. Legacy rows default to 0, i.e. they keep holding the
	// reservation they were created with (the ledger seed below counts them).
	if !uploadColumns["quota_released"] {
		if _, err := r.db.Exec("ALTER TABLE upload_sessions ADD COLUMN quota_released INTEGER NOT NULL DEFAULT 0"); err != nil {
			return fmt.Errorf("unable to migrate schema: %w", err)
		}
	}
	if _, err := r.db.Exec(uploadRequestIDIndex); err != nil {
		return fmt.Errorf("unable to migrate schema: %w", err)
	}
	// upload_quota single row (id=1); created above, seeded once here.
	if _, err := r.db.Exec(uploadQuotaSeedRow); err != nil {
		return fmt.Errorf("unable to migrate schema: %w", err)
	}
	return nil
}

// tableColumns returns the column-name set of a table.
func (r *ClipRepository) tableColumns(table string) (map[string]bool, error) {
	rows, err := r.db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return nil, fmt.Errorf("unable to inspect schema: %w", err)
	}
	defer rows.Close()
	columns := make(map[string]bool)
	for rows.Next() {
		var (
			cid      int
			name     string
			declType string
			notNull  int
			dflt     sql.NullString
			pk       int
		)
		if err := rows.Scan(&cid, &name, &declType, &notNull, &dflt, &pk); err != nil {
			return nil, fmt.Errorf("unable to inspect schema: %w", err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("unable to inspect schema: %w", err)
	}
	return columns, nil
}

func (r *ClipRepository) tokenTTLSeconds() int64 {
	hours := r.settings.TokenExpiryHours
	if hours < 1 {
		hours = 1
	}
	return int64(hours) * 60 * 60
}

func nowUnix() int64 { return time.Now().UTC().Unix() }

// RegisterToken mirrors ClipRepository.register_token.
func (r *ClipRepository) RegisterToken(token string, environmentID *string) (*TokenRecord, error) {
	trimmed := strings.TrimSpace(token)
	if trimmed == "" {
		return nil, apperr.NewValueErrorCode(apperr.CodeTokenInvalid, "持久 Token 无效")
	}
	now := nowUnix()
	ttl := r.tokenTTLSeconds()
	expiresAt := now + ttl
	var lastUsedAt *int64
	assignedOwner := ""

	r.mu.Lock()
	defer r.mu.Unlock()

	tx, err := r.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	var (
		ownerID   string
		updatedAt int64
		lastUsed  sql.NullInt64
		rowExpiry int64
	)
	row := tx.QueryRow("SELECT owner_id, updated_at, last_used_at, expires_at FROM tokens WHERE token = ?", trimmed)
	err = row.Scan(&ownerID, &updatedAt, &lastUsed, &rowExpiry)

	switch {
	case err == nil:
		if rowExpiry <= now {
			if environmentID != nil && *environmentID != "" && *environmentID == ownerID {
				assignedOwner = *environmentID
			} else {
				assignedOwner = uuid.NewString()
			}
			if _, execErr := tx.Exec(
				"UPDATE tokens SET owner_id = ?, updated_at = ?, last_used_at = NULL, expires_at = ? WHERE token = ?",
				assignedOwner, now, expiresAt, trimmed,
			); execErr != nil {
				return nil, execErr
			}
			lastUsedAt = nil
		} else {
			existingOwner := ownerID
			if environmentID != nil && *environmentID != "" && *environmentID == existingOwner {
				if _, execErr := tx.Exec(
					"UPDATE tokens SET updated_at = ?, expires_at = ? WHERE token = ?",
					now, expiresAt, trimmed,
				); execErr != nil {
					return nil, execErr
				}
				assignedOwner = existingOwner
				if lastUsed.Valid {
					value := lastUsed.Int64
					lastUsedAt = &value
				}
			} else {
				return nil, apperr.NewValueErrorCode(apperr.CodeTokenOccupied, "持久 Token 已被其他设备占用，请稍后重试")
			}
		}
	case errors.Is(err, sql.ErrNoRows):
		if environmentID != nil && *environmentID != "" {
			assignedOwner = *environmentID
		} else {
			assignedOwner = uuid.NewString()
		}
		if _, execErr := tx.Exec(
			"INSERT INTO tokens (token, owner_id, updated_at, last_used_at, expires_at) VALUES (?, ?, ?, NULL, ?)",
			trimmed, assignedOwner, now, expiresAt,
		); execErr != nil {
			if uniqueConstraintOn(execErr, "tokens.token") {
				return nil, apperr.NewValueErrorCode(apperr.CodeTokenOccupied, "持久 Token 已被其他设备占用，请稍后重试")
			}
			return nil, execErr
		}

		lastUsedAt = nil
	default:
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &TokenRecord{
		Token:         trimmed,
		EnvironmentID: assignedOwner,
		UpdatedAt:     now,
		LastUsedAt:    lastUsedAt,
		ExpiresAt:     expiresAt,
	}, nil
}

// EnsureTokenOwner mirrors ClipRepository.ensure_token_owner.
func (r *ClipRepository) EnsureTokenOwner(token string, environmentID string) (*TokenRecord, error) {
	trimmed := strings.TrimSpace(token)
	if trimmed == "" {
		return nil, apperr.NewValueErrorCode(apperr.CodeTokenInvalid, "持久 Token 无效")
	}
	normalizedEnv := strings.TrimSpace(environmentID)
	if normalizedEnv == "" {
		return nil, apperr.NewValueErrorCode(apperr.CodeTokenVerifyFailed, "Token 校验失败")
	}
	now := nowUnix()
	ttl := r.tokenTTLSeconds()
	newExpiry := now + ttl

	r.mu.Lock()
	defer r.mu.Unlock()

	tx, err := r.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	var (
		ownerID   string
		updatedAt int64
		lastUsed  sql.NullInt64
		rowExpiry int64
	)
	row := tx.QueryRow("SELECT owner_id, updated_at, last_used_at, expires_at FROM tokens WHERE token = ?", trimmed)
	if err := row.Scan(&ownerID, &updatedAt, &lastUsed, &rowExpiry); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, apperr.NewValueErrorCode(apperr.CodeTokenNotRegistered, "持久 Token 未注册，请重新保存")
		}
		return nil, err
	}
	if rowExpiry <= now {
		if _, err := tx.Exec("DELETE FROM tokens WHERE token = ?", trimmed); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, apperr.NewValueErrorCode(apperr.CodeTokenExpired, "持久 Token 已过期，请重新生成")
	}
	if ownerID != normalizedEnv {
		return nil, apperr.NewValueErrorCode(apperr.CodeTokenOccupied, "持久 Token 已被其他设备占用，请稍后重试")
	}
	if _, err := tx.Exec(
		"UPDATE tokens SET last_used_at = ?, expires_at = ? WHERE token = ?",
		now, newExpiry, trimmed,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	lastUsedAt := now
	return &TokenRecord{
		Token:         trimmed,
		EnvironmentID: normalizedEnv,
		UpdatedAt:     updatedAt,
		LastUsedAt:    &lastUsedAt,
		ExpiresAt:     newExpiry,
	}, nil
}

func scanClip(scan func(dest ...interface{}) error) (*models.Clip, error) {
	var (
		id            string
		clipType      string
		createdAt     int64
		expiresAt     int64
		maxDownloads  int
		downloadCount int
		accessCode    sql.NullString
		accessToken   sql.NullString
		ownerID       sql.NullString
		textContent   sql.NullString
		fileName      sql.NullString
		filePath      sql.NullString
		fileSize      sql.NullInt64
		fileMime      sql.NullString
	)
	if err := scan(&id, &clipType, &createdAt, &expiresAt, &maxDownloads, &downloadCount,
		&accessCode, &accessToken, &ownerID, &textContent, &fileName, &filePath, &fileSize, &fileMime); err != nil {
		return nil, err
	}
	clip := &models.Clip{
		ID:            id,
		Type:          clipType,
		CreatedAt:     time.Unix(createdAt, 0).UTC(),
		ExpiresAt:     time.Unix(expiresAt, 0).UTC(),
		MaxDownloads:  maxDownloads,
		DownloadCount: downloadCount,
		EnvironmentID: ownerID.String,
	}
	if accessCode.Valid {
		value := accessCode.String
		clip.AccessCode = &value
	}
	if accessToken.Valid {
		value := accessToken.String
		clip.AccessToken = &value
	}
	if textContent.Valid {
		value := textContent.String
		clip.Text = &value
	}
	if filePath.Valid && filePath.String != "" {
		clip.StoredFile = &models.StoredFile{
			Name: fileName.String,
			Size: fileSize.Int64,
			Mime: fileMime.String,
			Path: filePath.String,
		}
	}
	return clip, nil
}

// SanitizeMaxDownloads mirrors ClipRepository.sanitize_max_downloads.
func (r *ClipRepository) SanitizeMaxDownloads(value *int) int {
	if value == nil {
		return r.settings.DefaultMaxDownloads
	}
	normalized := *value
	if normalized < 1 {
		normalized = 1
	}
	if normalized > r.settings.MaxAllowedDownloads {
		normalized = r.settings.MaxAllowedDownloads
	}
	return normalized
}

// CreateClipParams groups the arguments of ClipRepository.create_clip.
type CreateClipParams struct {
	ClipType      string
	ExpiresAtMs   int64
	MaxDownloads  *int
	AccessCode    *string
	AccessToken   *string
	EnvironmentID string
	Text          *string
	StoredFile    *models.StoredFile
}

// CreateClip mirrors ClipRepository.create_clip.
func (r *ClipRepository) CreateClip(params CreateClipParams) (*models.Clip, error) {
	expiresAt := time.UnixMilli(params.ExpiresAtMs).UTC()
	if !expiresAt.After(time.Now().UTC()) {
		return nil, apperr.NewValueErrorCode(apperr.CodeInvalidExpiresAt, "过期时间必须晚于当前时间")
	}
	environmentID := strings.TrimSpace(params.EnvironmentID)
	if environmentID == "" {
		return nil, apperr.NewValueErrorCode(apperr.CodeMissingEnvironment, "剪贴板所属标识缺失")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	tx, err := r.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	if params.AccessCode != nil && *params.AccessCode != "" {
		var existingID string
		err := tx.QueryRow("SELECT id FROM clips WHERE access_code = ?", *params.AccessCode).Scan(&existingID)
		switch {
		case err == nil:
			return nil, apperr.NewValueErrorCode(apperr.CodeAccessCodeConflict, "直链码已存在，请刷新后再试")
		case errors.Is(err, sql.ErrNoRows):
		default:
			return nil, err
		}
	}

	clipID := uuid.NewString()
	createdAt := time.Now().UTC()

	var fileName, filePath, fileMime interface{}
	var fileSize interface{}
	if params.StoredFile != nil {
		fileName = params.StoredFile.Name
		filePath = params.StoredFile.Path
		fileSize = params.StoredFile.Size
		fileMime = params.StoredFile.Mime
	}

	if _, err := tx.Exec(`INSERT INTO clips (
			id, type, created_at, expires_at, max_downloads,
			download_count, access_code, access_token, owner_id, text_content,
			file_name, file_path, file_size, file_mime
		) VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?)`,
		clipID,
		params.ClipType,
		createdAt.Unix(),
		expiresAt.Unix(),
		r.SanitizeMaxDownloads(params.MaxDownloads),
		params.AccessCode,
		params.AccessToken,
		environmentID,
		params.Text,
		fileName,
		filePath,
		fileSize,
		fileMime,
	); err != nil {
		if uniqueConstraintOn(err, "clips.access_code") {
			return nil, apperr.NewValueErrorCode(apperr.CodeAccessCodeConflict, "直链码已存在，请刷新后再试")
		}
		return nil, err
	}

	clip, err := scanClip(tx.QueryRow("SELECT "+clipColumns+" FROM clips WHERE id = ?", clipID).Scan)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return clip, nil
}

// ListClips mirrors ClipRepository.list_clips.
func (r *ClipRepository) ListClips(environmentID string) ([]*models.Clip, error) {
	rows, err := r.db.Query(
		"SELECT "+clipColumns+" FROM clips WHERE owner_id = ? ORDER BY created_at DESC",
		environmentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	clips := make([]*models.Clip, 0)
	for rows.Next() {
		clip, err := scanClip(rows.Scan)
		if err != nil {
			return nil, err
		}
		clips = append(clips, clip)
	}
	return clips, rows.Err()
}

func (r *ClipRepository) queryClip(query string, args ...interface{}) (*models.Clip, error) {
	clip, err := scanClip(r.db.QueryRow(query, args...).Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return clip, nil
}

// GetClipByCode mirrors ClipRepository.get_clip_by_code.
func (r *ClipRepository) GetClipByCode(accessCode string) (*models.Clip, error) {
	return r.queryClip("SELECT "+clipColumns+" FROM clips WHERE access_code = ?", accessCode)
}

// GetClipByCodeAndOwner mirrors ClipRepository.get_clip_by_code_and_owner.
func (r *ClipRepository) GetClipByCodeAndOwner(accessCode string, environmentID string) (*models.Clip, error) {
	return r.queryClip("SELECT "+clipColumns+" FROM clips WHERE access_code = ? AND owner_id = ?", accessCode, environmentID)
}

// GetClip mirrors ClipRepository.get_clip.
func (r *ClipRepository) GetClip(clipID string) (*models.Clip, error) {
	return r.queryClip("SELECT "+clipColumns+" FROM clips WHERE id = ?", clipID)
}

// GetClipByToken mirrors ClipRepository.get_clip_by_token. An empty
// environmentID reproduces passing None from Python.
func (r *ClipRepository) GetClipByToken(accessToken string, environmentID string) (*models.Clip, error) {
	if environmentID != "" {
		return r.queryClip(
			"SELECT "+clipColumns+" FROM clips WHERE access_token = ? AND owner_id = ? ORDER BY created_at DESC",
			accessToken, environmentID,
		)
	}
	return r.queryClip(
		"SELECT "+clipColumns+" FROM clips WHERE access_token = ? ORDER BY created_at DESC",
		accessToken,
	)
}

// DeleteClip mirrors ClipRepository.delete_clip, including the file cleanup.
func (r *ClipRepository) DeleteClip(clipID string, environmentID string) (bool, error) {
	normalizedEnv := strings.TrimSpace(environmentID)
	if normalizedEnv == "" {
		return false, nil
	}

	r.mu.Lock()
	var filePath string
	err := func() error {
		tx, err := r.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback() //nolint:errcheck
		var (
			storedPath sql.NullString
			ownerID    string
		)
		row := tx.QueryRow("SELECT file_path, owner_id FROM clips WHERE id = ?", clipID)
		if err := row.Scan(&storedPath, &ownerID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return sql.ErrNoRows
			}
			return err
		}
		if ownerID != normalizedEnv {
			return sql.ErrNoRows
		}
		if _, err := tx.Exec("DELETE FROM clips WHERE id = ?", clipID); err != nil {
			return err
		}
		filePath = storedPath.String
		return tx.Commit()
	}()
	r.mu.Unlock()

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if filePath != "" {
		_ = os.Remove(filePath)
	}
	return true, nil
}

// IncrementDownloads mirrors ClipRepository.increment_downloads and returns the
// refreshed clip plus the "reached the download limit" flag.
func (r *ClipRepository) IncrementDownloads(clipID string, environmentID string) (*models.Clip, bool, error) {
	normalizedEnv := strings.TrimSpace(environmentID)
	if normalizedEnv == "" {
		return nil, false, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	tx, err := r.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback() //nolint:errcheck

	clip, err := scanClip(tx.QueryRow("SELECT "+clipColumns+" FROM clips WHERE id = ?", clipID).Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if clip.EnvironmentID != normalizedEnv {
		return nil, false, nil
	}
	newCount := clip.DownloadCount + 1
	if _, err := tx.Exec("UPDATE clips SET download_count = ? WHERE id = ?", newCount, clipID); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	clip.DownloadCount = newCount
	return clip, newCount >= clip.MaxDownloads, nil
}

// PurgeInactive mirrors ClipRepository.purge_inactive.
func (r *ClipRepository) PurgeInactive() (int, error) {
	now := nowUnix()

	r.mu.Lock()
	victims, err := r.collectInactive(now)
	if err != nil {
		r.mu.Unlock()
		return 0, err
	}
	if len(victims) == 0 {
		r.mu.Unlock()
		return 0, nil
	}

	tx, err := r.db.Begin()
	if err != nil {
		r.mu.Unlock()
		return 0, err
	}
	for _, victim := range victims {
		if _, err := tx.Exec("DELETE FROM clips WHERE id = ?", victim.id); err != nil {
			_ = tx.Rollback()
			r.mu.Unlock()
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		r.mu.Unlock()
		return 0, err
	}
	r.mu.Unlock()

	for _, victim := range victims {
		if victim.filePath != "" {
			_ = os.Remove(victim.filePath)
		}
	}
	return len(victims), nil
}

type pendingPurge struct {
	id       string
	filePath string
}

func (r *ClipRepository) collectInactive(now int64) ([]pendingPurge, error) {
	rows, err := r.db.Query(
		"SELECT id, file_path FROM clips WHERE expires_at <= ? OR download_count >= max_downloads",
		now,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var victims []pendingPurge
	for rows.Next() {
		var (
			id       string
			filePath sql.NullString
		)
		if err := rows.Scan(&id, &filePath); err != nil {
			return nil, err
		}
		victims = append(victims, pendingPurge{id: id, filePath: filePath.String})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return victims, nil
}

// uniqueConstraintOn reports whether err is a SQLite UNIQUE failure on tableColumn
// (for example "clips.access_code"). Used as a race fallback next to the
// explicit pre-insert existence check.
func uniqueConstraintOn(err error, tableColumn string) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") && strings.Contains(msg, tableColumn)
}
