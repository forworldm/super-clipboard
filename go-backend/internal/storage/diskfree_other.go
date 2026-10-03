//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !solaris

package storage

import "errors"

// FreeDiskBytes is unsupported on this platform. Callers treat an error as
// "watermark unknown" and fail open (uploads keep working), so the guard never
// blocks a deployment on an exotic OS.
func FreeDiskBytes(path string) (int64, error) {
	return 0, errors.New("free disk space probe is not supported on this platform")
}
