package api

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeJoinRoot(t *testing.T) {
	root := t.TempDir()

	target, ok := safeJoinRoot(root, "index.html")
	if !ok {
		t.Fatalf("expected index.html to join")
	}
	if filepath.Base(target) != "index.html" {
		t.Fatalf("unexpected target %q", target)
	}

	target, ok = safeJoinRoot(root, "/vendor..chunk.js")
	if !ok || filepath.Base(target) != "vendor..chunk.js" {
		t.Fatalf("dots in the filename should be allowed, got %q ok=%v", target, ok)
	}

	if _, ok := safeJoinRoot(root, ""); ok {
		t.Fatalf("empty remainder should be rejected")
	}

	for _, remainder := range []string{"../../../../etc/passwd", "/../../etc/passwd", "foo/../../../etc/passwd"} {
		joined, ok := safeJoinRoot(root, remainder)
		if !ok {
			continue
		}
		rel, err := filepath.Rel(root, joined)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Fatalf("joined path escaped root: remainder=%q target=%q rel=%q", remainder, joined, rel)
		}
	}
}
