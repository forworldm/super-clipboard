package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "github.com/mattn/go-sqlite3"
)

const createTablesSQL = `
CREATE TABLE IF NOT EXISTS clips (
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
);
CREATE INDEX IF NOT EXISTS idx_clips_expires_at ON clips(expires_at);
CREATE INDEX IF NOT EXISTS idx_clips_access_code ON clips(access_code);
CREATE INDEX IF NOT EXISTS idx_clips_access_token ON clips(access_token);
CREATE TABLE IF NOT EXISTS tokens (
    token TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    updated_at INTEGER NOT NULL,
    last_used_at INTEGER,
    expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tokens_expires_at ON tokens(expires_at);
`

type ClipRepository struct {
	db   *sql.DB
	lock sync.Mutex
	cfg  *Config
}

func NewClipRepository(cfg *Config) (*ClipRepository, error) {
	db, err := sql.Open("sqlite3", cfg.DatabasePath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	repo := &ClipRepository{db: db, cfg: cfg}
	if err := repo.ensureSchema(); err != nil {
		return nil, err
	}
	return repo, nil
}

func (r *ClipRepository) Close() error {
	return r.db.Close()
}

func (r *ClipRepository) ensureSchema() error {
	if _, err := r.db.Exec(createTablesSQL); err != nil {
		return err
	}
	// Check if owner_id column exists (for migration compatibility)
	rows, err := r.db.Query("PRAGMA table_info(clips)")
	if err != nil {
		return err
	}
	defer rows.Close()
	hasOwnerID := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt_value sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt_value, &pk); err != nil {
			return err
		}
		if name == "owner_id" {
			hasOwnerID = true
		}
	}
	if !hasOwnerID {
		if _, err := r.db.Exec("ALTER TABLE clips ADD COLUMN owner_id TEXT DEFAULT ''"); err != nil {
			return err
		}
		if _, err := r.db.Exec("UPDATE clips SET owner_id = '' WHERE owner_id IS NULL"); err != nil {
			return err
		}
	}
	return nil
}

func (r *ClipRepository) tokenTTLSeconds() int64 {
	hours := r.cfg.TokenExpiryHours
	if hours < 1 {
		hours = 1
	}
	return int64(hours) * 60 * 60
}

type TokenRecord struct {
	Token         string
	EnvironmentID string
	UpdatedAt     int64
	LastUsedAt    *int64
	ExpiresAt     int64
}

func (r *ClipRepository) RegisterToken(token, environmentID string) (*TokenRecord, error) {
	trimmed := trimString(token)
	if trimmed == "" {
		return nil, errors.New("持久 Token 无效")
	}
	now := time.Now().UTC().Unix()
	ttl := r.tokenTTLSeconds()
	expiresAt := now + ttl
	var lastUsedAt *int64

	r.lock.Lock()
	defer r.lock.Unlock()

	tx, err := r.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var existingOwner string
	var existingUpdated, existingLastUsed, existingExpires sql.NullInt64
	err = tx.QueryRow("SELECT owner_id, updated_at, last_used_at, expires_at FROM tokens WHERE token = ?", trimmed).
		Scan(&existingOwner, &existingUpdated, &existingLastUsed, &existingExpires)

	if err == nil {
		// token exists
		if existingExpires.Int64 <= now {
			// expired - reassign
			assignedOwner := ""
			if environmentID != "" && environmentID == existingOwner {
				assignedOwner = environmentID
			} else {
				assignedOwner = uuid.New().String()
			}
			if _, err := tx.Exec("UPDATE tokens SET owner_id = ?, updated_at = ?, last_used_at = NULL, expires_at = ? WHERE token = ?",
				assignedOwner, now, expiresAt, trimmed); err != nil {
				return nil, err
			}
			lastUsedAt = nil
			existingOwner = assignedOwner
		} else {
			if environmentID != "" && environmentID == existingOwner {
				if _, err := tx.Exec("UPDATE tokens SET updated_at = ?, expires_at = ? WHERE token = ?",
					now, expiresAt, trimmed); err != nil {
					return nil, err
				}
				if existingLastUsed.Valid {
					v := existingLastUsed.Int64
					lastUsedAt = &v
				}
			} else {
				return nil, errors.New("持久 Token 已被其他设备占用，请稍后重试")
			}
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		assignedOwner := environmentID
		if assignedOwner == "" {
			assignedOwner = uuid.New().String()
		}
		if _, err := tx.Exec("INSERT INTO tokens (token, owner_id, updated_at, last_used_at, expires_at) VALUES (?, ?, ?, NULL, ?)",
			trimmed, assignedOwner, now, expiresAt); err != nil {
			return nil, err
		}
		existingOwner = assignedOwner
		lastUsedAt = nil
	} else {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return &TokenRecord{
		Token:         trimmed,
		EnvironmentID: existingOwner,
		UpdatedAt:     now,
		LastUsedAt:    lastUsedAt,
		ExpiresAt:     expiresAt,
	}, nil
}

func (r *ClipRepository) EnsureTokenOwner(token, environmentID string) (*TokenRecord, error) {
	trimmed := trimString(token)
	if trimmed == "" {
		return nil, errors.New("持久 Token 无效")
	}
	normalizedEnv := trimString(environmentID)
	if normalizedEnv == "" {
		return nil, errors.New("Token 校验失败")
	}
	now := time.Now().UTC().Unix()
	newExpires := now + r.tokenTTLSeconds()

	r.lock.Lock()
	defer r.lock.Unlock()

	tx, err := r.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var ownerID string
	var updatedAt int64
	var expiresAt int64
	err = tx.QueryRow("SELECT owner_id, updated_at, expires_at FROM tokens WHERE token = ?", trimmed).
		Scan(&ownerID, &updatedAt, &expiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("持久 Token 未注册，请重新保存")
		}
		return nil, err
	}
	if expiresAt <= now {
		if _, err := tx.Exec("DELETE FROM tokens WHERE token = ?", trimmed); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, errors.New("持久 Token 已过期，请重新生成")
	}
	if ownerID != normalizedEnv {
		return nil, errors.New("持久 Token 已被其他设备占用，请稍后重试")
	}
	if _, err := tx.Exec("UPDATE tokens SET last_used_at = ?, expires_at = ? WHERE token = ?",
		now, newExpires, trimmed); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &TokenRecord{
		Token:         trimmed,
		EnvironmentID: normalizedEnv,
		UpdatedAt:     updatedAt,
		LastUsedAt:    &now,
		ExpiresAt:     newExpires,
	}, nil
}

func (r *ClipRepository) rowToClip(row *sql.Row) (*Clip, error) {
	var clip Clip
	var accessCode, accessToken, textContent sql.NullString
	var fileName, filePath, fileMime sql.NullString
	var fileSize sql.NullInt64
	var ownerID string
	var createdAt, expiresAt int64
	err := row.Scan(
		&clip.ID, &clip.Type,
		&createdAt, &expiresAt,
		&clip.MaxDownloads, &clip.DownloadCount,
		&accessCode, &accessToken,
		&ownerID,
		&textContent, &fileName, &filePath, &fileSize, &fileMime,
	)
	if err != nil {
		return nil, err
	}
	clip.CreatedAt = time.Unix(createdAt, 0).UTC()
	clip.ExpiresAt = time.Unix(expiresAt, 0).UTC()
	clip.AccessCode = accessCode.String
	clip.AccessToken = accessToken.String
	clip.EnvironmentID = ownerID
	clip.Text = textContent.String
	if filePath.Valid && filePath.String != "" {
		clip.StoredFile = &StoredFile{
			Name: fileName.String,
			Size: fileSize.Int64,
			Mime: fileMime.String,
			Path: filePath.String,
		}
	}
	return &clip, nil
}

// scanClip does the same from *sql.Rows
func (r *ClipRepository) scanClip(rows *sql.Rows) (*Clip, error) {
	var clip Clip
	var accessCode, accessToken, textContent sql.NullString
	var fileName, filePath, fileMime sql.NullString
	var fileSize sql.NullInt64
	var ownerID string
	var createdAt, expiresAt int64
	err := rows.Scan(
		&clip.ID, &clip.Type,
		&createdAt, &expiresAt,
		&clip.MaxDownloads, &clip.DownloadCount,
		&accessCode, &accessToken,
		&ownerID,
		&textContent, &fileName, &filePath, &fileSize, &fileMime,
	)
	if err != nil {
		return nil, err
	}
	clip.CreatedAt = time.Unix(createdAt, 0).UTC()
	clip.ExpiresAt = time.Unix(expiresAt, 0).UTC()
	clip.AccessCode = accessCode.String
	clip.AccessToken = accessToken.String
	clip.EnvironmentID = ownerID
	clip.Text = textContent.String
	if filePath.Valid && filePath.String != "" {
		clip.StoredFile = &StoredFile{
			Name: fileName.String,
			Size: fileSize.Int64,
			Mime: fileMime.String,
			Path: filePath.String,
		}
	}
	return &clip, nil
}

func (r *ClipRepository) SanitizeMaxDownloads(value *int) int {
	if value == nil {
		return r.cfg.DefaultMaxDownloads
	}
	n := *value
	if n < 1 {
		n = 1
	}
	if n > r.cfg.MaxAllowedDownloads {
		n = r.cfg.MaxAllowedDownloads
	}
	return n
}

type CreateClipInput struct {
	Type          string
	ExpiresAtMS   int64
	MaxDownloads  *int
	AccessCode    string
	AccessToken   string
	EnvironmentID string
	Text          string
	StoredFile    *StoredFile
}

func (r *ClipRepository) CreateClip(input *CreateClipInput) (*Clip, error) {
	expiresSec := float64(input.ExpiresAtMS) / 1000.0
	expiresAt := time.Unix(int64(expiresSec), 0).UTC()
	if !expiresAt.After(time.Now().UTC()) {
		return nil, errors.New("过期时间必须晚于当前时间")
	}
	envID := trimString(input.EnvironmentID)
	if envID == "" {
		return nil, errors.New("剪贴板所属标识缺失")
	}

	r.lock.Lock()
	defer r.lock.Unlock()

	tx, err := r.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if input.AccessCode != "" {
		var existing string
		err := tx.QueryRow("SELECT id FROM clips WHERE access_code = ?", input.AccessCode).Scan(&existing)
		if err == nil {
			return nil, errors.New("直链码已存在，请刷新后再试")
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}

	clipID := uuid.New().String()
	createdAt := time.Now().UTC()
	maxDL := r.SanitizeMaxDownloads(input.MaxDownloads)

	var fileName, filePath, fileMime sql.NullString
	var fileSize sql.NullInt64
	var textContent sql.NullString
	if input.StoredFile != nil {
		fileName = sql.NullString{String: input.StoredFile.Name, Valid: true}
		filePath = sql.NullString{String: input.StoredFile.Path, Valid: true}
		fileSize = sql.NullInt64{Int64: input.StoredFile.Size, Valid: true}
		fileMime = sql.NullString{String: input.StoredFile.Mime, Valid: true}
	}
	if input.Type == "text" {
		textContent = sql.NullString{String: input.Text, Valid: true}
	}

	_, err = tx.Exec(`
		INSERT INTO clips (
			id, type, created_at, expires_at, max_downloads,
			download_count, access_code, access_token, owner_id, text_content,
			file_name, file_path, file_size, file_mime
		) VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?)`,
		clipID, input.Type,
		int(createdAt.Unix()), int(expiresAt.Unix()),
		maxDL,
		nilIfEmpty(input.AccessCode), nilIfEmpty(input.AccessToken),
		envID,
		textContent,
		fileName, filePath, fileSize, fileMime,
	)
	if err != nil {
		return nil, err
	}

	row := tx.QueryRow("SELECT * FROM clips WHERE id = ?", clipID)
	clip, err := r.rowToClip(row)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return clip, nil
}

func nilIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func (r *ClipRepository) ListClips(environmentID string) ([]*Clip, error) {
	rows, err := r.db.Query("SELECT * FROM clips WHERE owner_id = ? ORDER BY created_at DESC", environmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var clips []*Clip
	for rows.Next() {
		clip, err := r.scanClip(rows)
		if err != nil {
			return nil, err
		}
		clips = append(clips, clip)
	}
	return clips, nil
}

func (r *ClipRepository) GetClipByCode(accessCode string) (*Clip, error) {
	row := r.db.QueryRow("SELECT * FROM clips WHERE access_code = ?", accessCode)
	clip, err := r.rowToClip(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return clip, nil
}

func (r *ClipRepository) GetClipByCodeAndOwner(accessCode, environmentID string) (*Clip, error) {
	row := r.db.QueryRow("SELECT * FROM clips WHERE access_code = ? AND owner_id = ?", accessCode, environmentID)
	clip, err := r.rowToClip(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return clip, nil
}

func (r *ClipRepository) GetClip(clipID string) (*Clip, error) {
	row := r.db.QueryRow("SELECT * FROM clips WHERE id = ?", clipID)
	clip, err := r.rowToClip(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return clip, nil
}

func (r *ClipRepository) GetClipByToken(accessToken string, environmentID string) (*Clip, error) {
	var row *sql.Row
	if environmentID != "" {
		row = r.db.QueryRow("SELECT * FROM clips WHERE access_token = ? AND owner_id = ? ORDER BY created_at DESC", accessToken, environmentID)
	} else {
		row = r.db.QueryRow("SELECT * FROM clips WHERE access_token = ? ORDER BY created_at DESC", accessToken)
	}
	clip, err := r.rowToClip(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return clip, nil
}

func (r *ClipRepository) DeleteClip(clipID, environmentID string) (bool, error) {
	normalizedEnv := trimString(environmentID)
	if normalizedEnv == "" {
		return false, nil
	}

	r.lock.Lock()
	defer r.lock.Unlock()

	var ownerID string
	var filePath sql.NullString
	err := r.db.QueryRow("SELECT file_path, owner_id FROM clips WHERE id = ?", clipID).
		Scan(&filePath, &ownerID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if ownerID != normalizedEnv {
		return false, nil
	}
	if _, err := r.db.Exec("DELETE FROM clips WHERE id = ?", clipID); err != nil {
		return false, err
	}
	if filePath.Valid && filePath.String != "" {
		os.Remove(filePath.String)
	}
	return true, nil
}

func (r *ClipRepository) IncrementDownloads(clipID, environmentID string) (*Clip, bool, error) {
	normalizedEnv := trimString(environmentID)
	if normalizedEnv == "" {
		return nil, false, nil
	}

	r.lock.Lock()
	defer r.lock.Unlock()

	row := r.db.QueryRow("SELECT * FROM clips WHERE id = ?", clipID)
	clip, err := r.rowToClip(row)
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
	if _, err := r.db.Exec("UPDATE clips SET download_count = ? WHERE id = ?", newCount, clipID); err != nil {
		return nil, false, err
	}
	clip.DownloadCount = newCount
	reached := newCount >= clip.MaxDownloads
	return clip, reached, nil
}

func (r *ClipRepository) PurgeInactive() (int, error) {
	now := int(time.Now().UTC().Unix())

	r.lock.Lock()
	defer r.lock.Unlock()

	rows, err := r.db.Query("SELECT id, file_path FROM clips WHERE expires_at <= ? OR download_count >= max_downloads", now)
	if err != nil {
		return 0, err
	}
	type purgeRow struct {
		id       string
		filePath string
	}
	var toPurge []purgeRow
	for rows.Next() {
		var id string
		var fp sql.NullString
		if err := rows.Scan(&id, &fp); err != nil {
			rows.Close()
			return 0, err
		}
		toPurge = append(toPurge, purgeRow{id: id, filePath: fp.String})
	}
	rows.Close()

	for _, pr := range toPurge {
		if _, err := r.db.Exec("DELETE FROM clips WHERE id = ?", pr.id); err != nil {
			return len(toPurge), err
		}
		if pr.filePath != "" {
			os.Remove(pr.filePath)
		}
	}
	return len(toPurge), nil
}

// Helper to parse string for optional int query params
func parseIntQuery(s string) (*int, error) {
	s = trimString(s)
	if s == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return nil, fmt.Errorf("invalid integer: %w", err)
	}
	return &n, nil
}

// trimString is a helper that trims spaces.
func trimString(s string) string {
	return trimSpace(s)
}

// ensure unused import paths don't cause issues
var _ = filepath.Separator
