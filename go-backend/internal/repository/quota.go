// Package repository -- global upload disk quota ledger.
//
// upload_quota is a single row (id = 1) holding the bytes reserved by live
// upload sessions. Every mutation is a conditional UPDATE (compare-and-swap)
// inside a short transaction, so concurrent inits can never oversell the
// configured budget and a racing abort/expire/complete can never return the
// same reservation twice. No global lock is taken beyond the repository mutex
// that already guards DB transactions, and no file IO happens here.
package repository

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/pixia1234/super-clipboard/backend/internal/apperr"
)

// UploadQuota mirrors the single row of upload_quota.
type UploadQuota struct {
	ReservedBytes int64
	UpdatedAt     int64
}

// GetUploadQuota reads the ledger (lock-free read).
func (r *ClipRepository) GetUploadQuota() (*UploadQuota, error) {
	var quota UploadQuota
	if err := r.db.QueryRow(
		"SELECT reserved_bytes, updated_at FROM upload_quota WHERE id = 1").Scan(
		&quota.ReservedBytes, &quota.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("upload quota ledger row is missing")
		}
		return nil, err
	}
	return &quota, nil
}

// UploadQuotaValue is a test/logging helper returning the reserved bytes.
func (r *ClipRepository) UploadQuotaValue() (int64, error) {
	quota, err := r.GetUploadQuota()
	if err != nil {
		return 0, err
	}
	return quota.ReservedBytes, nil
}

// reservationsHeldPredicate is the ONE authoritative definition of "this row
// still holds its upload quota reservation" and therefore of "the ledger must
// count these file_size bytes".
//
// The predicate is quota_released ALONE, on purpose -- never `status`:
//
//   - `active`     holds its reservation (bytes are in the pre-allocated file);
//   - `completing` ALSO holds it: the state flip is a pure concurrency guard
//     taken before the clip INSERT, and releaseQuotaTx only runs on the
//     terminal transitions (complete success, abort, expiry purge). A ledger
//     query filtered with `status = 'active'` would therefore miss completing
//     sessions, and RecomputeUploadQuota would "repair" that missing amount
//     away -- silently freeing bytes another upload could reserve twice.
//   - `completed` carries quota_released = 1 (released inside the very same
//     transaction as the status flip), so it is excluded by the flag itself.
//
// Keep this predicate in sync with releaseQuotaTx (which CASes exactly on it)
// and with uploadQuotaSeedRow in repository.go.
const reservationsHeldPredicate = `quota_released = 0`

// CountActiveUploadSessions counts sessions in the `active` state. It is a
// DIAGNOSTIC counter only: `completing` sessions are not `active` yet they do
// hold their reservation, so this number must never be compared against the
// ledger. Use CountUploadSessionsHoldingQuota for quota decisions.
func (r *ClipRepository) CountActiveUploadSessions() (int64, error) {
	var count int64
	if err := r.db.QueryRow(
		"SELECT COUNT(*) FROM upload_sessions WHERE status = ?", UploadStatusActive).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// CountUploadSessionsHoldingQuota counts the sessions that still hold an upload
// quota reservation (active + completing). It is the count behind the
// MaxActiveUploadSessions gate: the resource that cap protects is a reserved
// slot in the ledger, and a completing session still owns one until its
// terminal transition.
func (r *ClipRepository) CountUploadSessionsHoldingQuota() (int64, error) {
	var count int64
	if err := r.db.QueryRow(
		"SELECT COUNT(*) FROM upload_sessions WHERE " + reservationsHeldPredicate).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// ExpectedReservedBytes is the convergent value of the ledger: SUM(file_size)
// over every session that still holds its reservation (active + completing).
func (r *ClipRepository) ExpectedReservedBytes() (int64, error) {
	var sum int64
	if err := r.db.QueryRow(
		"SELECT COALESCE(SUM(file_size), 0) FROM upload_sessions WHERE " + reservationsHeldPredicate).Scan(&sum); err != nil {
		return 0, err
	}
	return sum, nil
}

// UploadQuotaDrift reports the ledger and the value it must equal. It never
// writes anything (diagnostics + tests).
func (r *ClipRepository) UploadQuotaDrift() (ledger int64, expected int64, err error) {
	if ledger, err = r.UploadQuotaValue(); err != nil {
		return 0, 0, err
	}
	if expected, err = r.ExpectedReservedBytes(); err != nil {
		return 0, 0, err
	}
	return ledger, expected, nil
}

// ReserveUploadQuota atomically moves fileSize into the ledger.
//
// The CAS predicate (`reserved_bytes + ? <= ?`) is evaluated by SQLite inside
// the UPDATE, so N concurrent inits against a budget that fits M of them let
// exactly M through; the rest see RowsAffected == 0 and get a typed 507 error.
// limit <= 0 means "unlimited" and only appends the reservation.
func (r *ClipRepository) ReserveUploadQuota(fileSize int64, limit int64) error {
	if fileSize < 0 {
		return errors.New("file size must be >= 0")
	}
	if limit < 0 {
		return errors.New("upload quota limit must be >= 0")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := nowUnix()
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var (
		res      sql.Result
		stmtErr  error
		reserved int64
	)
	if limit > 0 {
		res, stmtErr = tx.Exec(`UPDATE upload_quota SET reserved_bytes = reserved_bytes + ?, updated_at = ?
			WHERE id = 1 AND reserved_bytes >= 0 AND reserved_bytes + ? <= ?`,
			fileSize, now, fileSize, limit)
	} else {
		res, stmtErr = tx.Exec(`UPDATE upload_quota SET reserved_bytes = reserved_bytes + ?, updated_at = ?
			WHERE id = 1 AND reserved_bytes >= 0`, fileSize, now)
	}
	if stmtErr != nil {
		return stmtErr
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 1 {
		return tx.Commit()
	}
	// CAS refused: either the budget is exhausted or the ledger row is gone.
	_ = tx.QueryRow("SELECT reserved_bytes FROM upload_quota WHERE id = 1").Scan(&reserved)
	if err := tx.Rollback(); err != nil {
		return err
	}
	if limit <= 0 {
		return errors.New("upload quota ledger row is missing")
	}
	return &apperr.StorageError{
		Code:      apperr.StorageCodeQuota,
		Message:   fmt.Sprintf("上传配额不足：已预留 %d 字节，再预留 %d 字节将超出总量配额 %d 字节", reserved, fileSize, limit),
		Requested: fileSize,
		Used:      reserved,
		Limit:     limit,
	}
}

// CompensateUploadQuota returns a reservation that never became a session row
// (init failed after reserving: disk watermark, insert error, idempotency race).
// The `reserved_bytes >= ?` predicate keeps the ledger from ever going negative.
func (r *ClipRepository) CompensateUploadQuota(fileSize int64) (bool, error) {
	if fileSize < 0 {
		return false, errors.New("file size must be >= 0")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	res, err := r.db.Exec(`UPDATE upload_quota SET reserved_bytes = reserved_bytes - ?, updated_at = ?
		WHERE id = 1 AND reserved_bytes >= ?`, fileSize, nowUnix(), fileSize)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

// releaseQuotaTx performs the idempotent release inside an existing transaction:
//
//	UPDATE upload_sessions SET quota_released = 1 WHERE id = ? AND quota_released = 0
//
// Only when that CAS wins (RowsAffected == 1) is the ledger decremented, so a
// session can never return its reservation twice no matter how abort, expiry
// cleanup and complete race. The CAS predicate is exactly
// reservationsHeldPredicate, i.e. the ledger query and the release agree on
// what "holds a reservation" means. Callers must hold r.mu.
func (r *ClipRepository) releaseQuotaTx(tx *sql.Tx, uploadID string, fileSize int64) (bool, error) {
	if fileSize < 0 {
		return false, errors.New("file size must be >= 0")
	}
	now := nowUnix()
	res, err := tx.Exec(`UPDATE upload_sessions SET quota_released = 1, updated_at = ?
		WHERE id = ? AND `+reservationsHeldPredicate, now, uploadID)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected != 1 {
		return false, nil // already released (or the row is gone): nothing to do
	}
	if _, err := tx.Exec(`UPDATE upload_quota SET reserved_bytes = reserved_bytes - ?, updated_at = ?
		WHERE id = 1 AND reserved_bytes >= ?`, fileSize, now, fileSize); err != nil {
		return false, err
	}
	return true, nil
}

// ReleaseUploadQuota releases the reservation of one session in its own
// transaction (complete-success path). Returns whether this call won the CAS.
func (r *ClipRepository) ReleaseUploadQuota(uploadID string, fileSize int64) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tx, err := r.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck
	released, err := r.releaseQuotaTx(tx, uploadID, fileSize)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return released, nil
}

// RecomputeUploadQuota is the cleanup-worker reconciliation: it recomputes the
// reservation from the sessions that still hold one and overwrites the ledger,
// repairing any drift left by a crash between "reserve" and "insert".
//
//	recompute = SUM(file_size) FROM upload_sessions WHERE quota_released = 0
//
// The predicate is the release CAS flag ALONE (see reservationsHeldPredicate):
// every status that still owns a reservation is summed, `active` AND
// `completing`. Adding a `status = 'active'` filter here would be a bug, not a
// tightening: it would drop the rows whose status was already flipped to
// `completing` (which happens BEFORE the clip INSERT and therefore before the
// release) and the repair would then write a too-small ledger -- silently
// handing those bytes out to the next init while the original session still
// owns its file. Only the counter is repaired -- no file is touched.
func (r *ClipRepository) RecomputeUploadQuota() (recomputed int64, previous int64, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tx, err := r.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := tx.QueryRow(`SELECT COALESCE(SUM(file_size), 0) FROM upload_sessions
		WHERE ` + reservationsHeldPredicate).Scan(&recomputed); err != nil {
		return 0, 0, err
	}
	if err := tx.QueryRow("SELECT reserved_bytes FROM upload_quota WHERE id = 1").Scan(&previous); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, errors.New("upload quota ledger row is missing")
		}
		return 0, 0, err
	}
	if recomputed != previous {
		if _, err := tx.Exec("UPDATE upload_quota SET reserved_bytes = ?, updated_at = ? WHERE id = 1",
			recomputed, nowUnix()); err != nil {
			return 0, 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return recomputed, previous, nil
}

// ForceUploadQuotaForTest overwrites the ledger (drift-injection tests only).
func (r *ClipRepository) ForceUploadQuotaForTest(reservedBytes int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.db.Exec("UPDATE upload_quota SET reserved_bytes = ?, updated_at = ? WHERE id = 1",
		reservedBytes, nowUnix())
	return err
}

// CheckDiskWatermark returns a typed 507 error when freeBytes dropped below the
// configured watermark. minFreeBytes <= 0 disables the guard. It is a pure
// function so api tests can inject any free-space probe result.
func CheckDiskWatermark(freeBytes int64, minFreeBytes int64) error {
	if minFreeBytes <= 0 {
		return nil
	}
	if freeBytes >= minFreeBytes {
		return nil
	}
	return &apperr.StorageError{
		Code:      apperr.StorageCodeDisk,
		Message:   fmt.Sprintf("磁盘可用空间不足：剩余 %d 字节，低于最低水位 %d 字节", freeBytes, minFreeBytes),
		Requested: freeBytes,
		Free:      freeBytes,
		Limit:     minFreeBytes,
	}
}
