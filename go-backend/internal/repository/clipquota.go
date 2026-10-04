// Package repository -- global stored-clip (saved file) quota ledger.
//
// `upload_quota` bounds what in-flight chunked upload sessions RESERVE, i.e.
// bytes that are still being streamed and may never become a clip. Once a clip
// is saved the upload reservation is returned (complete success) and the bytes
// move on to live forever inside the clips table -- exactly the bytes that used
// to be unaccounted for.
//
// `clip_quota` closes that hole: a single row (id = 1) holding
// SUM(file_size) over every row of `clips`. Every mutation is a conditional
// UPDATE (compare-and-swap) executed INSIDE the very same transaction that
// inserts or deletes the clip row, so:
//
//   - a clip whose bytes do not fit the budget is never persisted (the INSERT
//     and the ledger charge roll back together);
//   - a deleted clip always returns exactly the bytes it was charged:
//     DeleteClip, PurgeInactive (TTL expiry, download limit) and the admin
//     delete endpoint all funnel through the same release helper;
//   - the counter can never go negative (`used_bytes >= ?` predicate) and can
//     never oversell (`used_bytes + ? <= ?` predicate).
//
// The ledger is bookkeeping only: it tracks the bytes of clips that really
// exist, and converges to the configured budget. It deliberately does NOT
// police chunk traffic (that is upload_quota's job) -- the charge happens at
// the single moment a clip row becomes durable.
package repository

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/pixia1234/super-clipboard/backend/internal/apperr"
)

// ClipQuota mirrors the single row of clip_quota.
type ClipQuota struct {
	UsedBytes int64
	UpdatedAt int64
}

// GetClipQuota reads the ledger (lock-free read).
func (r *ClipRepository) GetClipQuota() (*ClipQuota, error) {
	var quota ClipQuota
	if err := r.db.QueryRow(
		"SELECT used_bytes, updated_at FROM clip_quota WHERE id = 1").Scan(
		&quota.UsedBytes, &quota.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("clip quota ledger row is missing")
		}
		return nil, err
	}
	return &quota, nil
}

// ClipQuotaValue is the value/assertion helper used by tests and logs.
func (r *ClipRepository) ClipQuotaValue() (int64, error) {
	quota, err := r.GetClipQuota()
	if err != nil {
		return 0, err
	}
	return quota.UsedBytes, nil
}

// StoredClipBytes computes the authoritative value of the ledger directly from
// the clips table (lock-free read). It is the convergent value every repair
// path uses.
func (r *ClipRepository) StoredClipBytes() (int64, error) {
	var total int64
	if err := r.db.QueryRow("SELECT COALESCE(SUM(file_size), 0) FROM clips").Scan(&total); err != nil {
		return 0, err
	}
	return total, nil
}

// ensureClipQuotaRowTx returns the current ledger value, creating the single row
// when it is missing (a pre-quota database opened by an older binary, or a
// hand-edited one). The seeded balance is SUM(file_size) over the stored clips:
// the convergent value, so an upgrade neither grants free credit nor forgets the
// bytes that are already on disk. Callers must hold r.mu.
func (r *ClipRepository) ensureClipQuotaRowTx(tx *sql.Tx) (int64, error) {
	var used int64
	err := tx.QueryRow("SELECT used_bytes FROM clip_quota WHERE id = 1").Scan(&used)
	if err == nil {
		return used, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if _, err := tx.Exec(clipQuotaSeedRow); err != nil {
		return 0, err
	}
	if err := tx.QueryRow("SELECT used_bytes FROM clip_quota WHERE id = 1").Scan(&used); err != nil {
		return 0, err
	}
	return used, nil
}

// reserveClipQuotaTx charges fileSize to the ledger inside tx and must be called
// in the same transaction as the clip INSERT.
//
// The CAS predicate (`used_bytes + ? <= ?`) is evaluated by SQLite inside the
// UPDATE, so concurrent inserts against a budget that fits M of them let exactly
// M through; the rest see RowsAffected == 0 and get a typed 507 error.
// limit <= 0 means "unlimited": the ledger still accumulates so an operator can
// switch the quota on later without a recompute. Callers must hold r.mu.
func (r *ClipRepository) reserveClipQuotaTx(tx *sql.Tx, fileSize int64, limit int64) error {
	if fileSize < 0 {
		return errors.New("file size must be >= 0")
	}
	if limit < 0 {
		return errors.New("clip quota limit must be >= 0")
	}
	if _, err := r.ensureClipQuotaRowTx(tx); err != nil {
		return err
	}
	now := nowUnix()
	var (
		res     sql.Result
		stmtErr error
	)
	if limit > 0 {
		res, stmtErr = tx.Exec(`UPDATE clip_quota SET used_bytes = used_bytes + ?, updated_at = ?
			WHERE id = 1 AND used_bytes >= 0 AND used_bytes + ? <= ?`,
			fileSize, now, fileSize, limit)
	} else {
		res, stmtErr = tx.Exec(`UPDATE clip_quota SET used_bytes = used_bytes + ?, updated_at = ?
			WHERE id = 1 AND used_bytes >= 0`, fileSize, now)
	}
	if stmtErr != nil {
		return stmtErr
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 1 {
		return nil
	}
	// CAS refused: the budget is exhausted (the row itself is guaranteed by
	// ensureClipQuotaRowTx). Report the numbers behind the refusal.
	var used int64
	if err := tx.QueryRow("SELECT used_bytes FROM clip_quota WHERE id = 1").Scan(&used); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("clip quota ledger row is missing")
		}
		return err
	}
	if limit <= 0 {
		return errors.New("clip quota ledger row is missing")
	}
	return &apperr.StorageError{
		Code:      apperr.StorageCodeClipQuota,
		Message:   fmt.Sprintf("存储配额不足：已保存 %d 字节，再保存 %d 字节将超出总量配额 %d 字节", used, fileSize, limit),
		Requested: fileSize,
		Used:      used,
		Limit:     limit,
	}
}

// releaseClipQuotaTx returns fileSize to the ledger inside tx. It is the exact
// inverse of reserveClipQuotaTx: the `used_bytes >= ?` predicate keeps the
// counter from going negative when a release is duplicated (a retried delete,
// a purge that races a manual delete) or when the ledger was already repaired.
// A missing ledger row is a no-op: the next charge re-seeds it from the clips
// table, which already accounts for the row being deleted right now.
// Callers must hold r.mu.
func (r *ClipRepository) releaseClipQuotaTx(tx *sql.Tx, fileSize int64) error {
	if fileSize <= 0 {
		return nil
	}
	if _, err := tx.Exec(`UPDATE clip_quota SET used_bytes = used_bytes - ?, updated_at = ?
		WHERE id = 1 AND used_bytes >= ?`, fileSize, nowUnix(), fileSize); err != nil {
		return err
	}
	return nil
}

// RecomputeClipQuota is the cleanup-worker reconciliation: it recomputes the
// saved-clip bytes from the clips table and overwrites the ledger, repairing any
// drift left by a crash between "charge" and "commit" (or by an operator editing
// the database by hand). Only the counter is repaired -- no clip and no file is
// touched.
func (r *ClipRepository) RecomputeClipQuota() (recomputed int64, previous int64, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tx, err := r.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := tx.QueryRow("SELECT COALESCE(SUM(file_size), 0) FROM clips").Scan(&recomputed); err != nil {
		return 0, 0, err
	}
	previous, err = r.ensureClipQuotaRowTx(tx)
	if err != nil {
		return 0, 0, err
	}
	if recomputed != previous {
		if _, err := tx.Exec("UPDATE clip_quota SET used_bytes = ?, updated_at = ? WHERE id = 1",
			recomputed, nowUnix()); err != nil {
			return 0, 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return recomputed, previous, nil
}

// ForceClipQuotaForTest overwrites the ledger (drift-injection tests only).
func (r *ClipRepository) ForceClipQuotaForTest(usedBytes int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := r.ensureClipQuotaRowTx(tx); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE clip_quota SET used_bytes = ?, updated_at = ? WHERE id = 1",
		usedBytes, nowUnix()); err != nil {
		return err
	}
	return tx.Commit()
}
