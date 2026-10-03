//go:build linux || darwin || freebsd || netbsd || openbsd || solaris

package storage

import (
	"os"
	"path/filepath"
	"syscall"
)

// FreeDiskBytes reports the bytes available to unprivileged writers on the
// filesystem holding path (statfs Bavail * Bsize).
//
// The path does not need to exist yet: the probe walks up to the closest
// existing ancestor so a not-yet-created storage dir still measures the volume
// it will live on.
func FreeDiskBytes(path string) (int64, error) {
	target := nearestExisting(path)
	var st syscall.Statfs_t
	if err := syscall.Statfs(target, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// nearestExisting walks up from path until it finds an existing entry.
func nearestExisting(path string) string {
	current := path
	for {
		if current == "" {
			return "."
		}
		if _, err := os.Stat(current); err == nil {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			return current
		}
		current = parent
	}
}
