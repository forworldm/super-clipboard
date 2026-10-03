package storage

import (
	"path/filepath"
	"testing"
)

// TestFreeDiskBytes exercises the real statfs probe, including the "path does
// not exist yet" case (the upload dir is created after the watermark check).
func TestFreeDiskBytes(t *testing.T) {
	dir := t.TempDir()
	free, err := FreeDiskBytes(dir)
	if err != nil {
		t.Skipf("free disk probe unsupported on this platform: %v", err)
	}
	if free < 0 {
		t.Fatalf("free bytes must be >= 0, got %d", free)
	}
	missing := filepath.Join(dir, "does", "not", "exist", "yet")
	nested, err := FreeDiskBytes(missing)
	if err != nil {
		t.Fatalf("probe of a missing path must walk up to an existing ancestor: %v", err)
	}
	if nested < 0 {
		t.Fatalf("free bytes must be >= 0, got %d", nested)
	}
}
