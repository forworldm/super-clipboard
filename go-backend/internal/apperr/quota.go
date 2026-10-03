package apperr

import "fmt"

// StorageCode identifies which global storage guard refused a request. The API
// layer renders every one of them as HTTP 507 (Insufficient Storage) but keeps
// the codes distinct so logs, metrics and clients can tell them apart.
type StorageCode string

const (
	// StorageCodeQuota means upload_quota.reserved_bytes would exceed the
	// configured UploadTotalQuotaBytes.
	StorageCodeQuota StorageCode = "upload_quota_exceeded"
	// StorageCodeSessionLimit means MaxActiveUploadSessions live sessions exist.
	StorageCodeSessionLimit StorageCode = "upload_session_limit"
	// StorageCodeDisk means free disk dropped below MinFreeDiskBytes.
	StorageCodeDisk StorageCode = "disk_watermark"
)

// StorageError is the typed error behind every 507 answer of the chunked
// upload path. It carries the numbers behind the refusal so the caller can see
// how far over the line the request was.
type StorageError struct {
	Code      StorageCode
	Message   string
	Requested int64 // bytes asked for (quota / disk guards)
	Used      int64 // bytes already reserved (quota guard)
	Active    int64 // live sessions (session guard)
	Limit     int64 // configured ceiling
	Free      int64 // bytes actually free on disk (disk guard)
}

// NewStorageError builds a StorageError with a formatted message.
func NewStorageError(code StorageCode, format string, args ...interface{}) *StorageError {
	return &StorageError{Code: code, Message: fmt.Sprintf(format, args...)}
}

func (e *StorageError) Error() string { return e.Message }

// HTTPStatus is always 507 for storage guards.
func (e *StorageError) HTTPStatus() int { return 507 }
