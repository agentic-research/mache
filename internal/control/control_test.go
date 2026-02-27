package control

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenOrCreate_SecurityValidation(t *testing.T) {
	tmpDir := t.TempDir()

	// Blocked path (outside safe areas)
	blockedPath := "/etc/mache.ctrl"
	_, err := OpenOrCreate(blockedPath)
	if err == nil {
		t.Errorf("Expected error for blocked path %s, but got nil", blockedPath)
	} else if !strings.Contains(err.Error(), "security violation") {
		t.Errorf("Expected security violation error, but got: %v", err)
	}

	// Safe path (inside TempDir)
	safePath := filepath.Join(tmpDir, "control.ctrl")
	ctrl, err := OpenOrCreate(safePath)
	if err != nil {
		if strings.Contains(err.Error(), "security violation") {
			t.Errorf("Path %s should be safe, but got security violation: %v", safePath, err)
		}
	}
	if ctrl != nil {
		_ = ctrl.Close()
	}

	// Path traversal attempt
	tempDirRoot := os.TempDir()
	traversalPath := filepath.Join(tempDirRoot, "..", "mache_evil.ctrl")

	_, err = OpenOrCreate(traversalPath)
	if err == nil {
		abs, _ := filepath.Abs(traversalPath)
		if !isUnder(abs, tempDirRoot) {
			t.Errorf("Expected error for traversal path %s escaping %s, but got nil", abs, tempDirRoot)
		}
	}
}

func TestIsUnder(t *testing.T) {
	tests := []struct {
		path     string
		base     string
		expected bool
	}{
		{"/tmp/foo", "/tmp", true},
		{"/tmp/foo/bar", "/tmp", true},
		{"/tmp", "/tmp", true},
		{"/etc/passwd", "/tmp", false},
		{"/tmp/../etc/passwd", "/tmp", false},
		{"/tmp-other/foo", "/tmp", false},
	}

	for _, tt := range tests {
		res := isUnder(tt.path, tt.base)
		if res != tt.expected {
			t.Errorf("isUnder(%q, %q) = %v; want %v", tt.path, tt.base, res, tt.expected)
		}
	}
}
