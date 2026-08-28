package leylinegraph

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAutoInvokeLeylineParse_NoBinary verifies the helper returns a clear
// error when leyline is not on PATH, not in the bundled location, and
// auto-download is opted out (MACHE_NO_LEYLINE). Without the opt-out the
// helper would now attempt a network download (its intended behavior), so the
// env var keeps this test hermetic.
func TestAutoInvokeLeylineParse_NoBinary(t *testing.T) {
	// Hide both PATH and the bundled location, and opt out of auto-download.
	t.Setenv("PATH", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MACHE_NO_LEYLINE", "1")

	_, _, err := AutoInvokeLeylineParse(t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MACHE_NO_LEYLINE")
}

// TestAutoInvokeLeylineParse_Success skips when leyline is unavailable.
// When available, parses a tiny Go source tree and verifies a non-empty
// .db is produced and cleanup removes it.
func TestAutoInvokeLeylineParse_Success(t *testing.T) {
	if _, err := exec.LookPath("leyline"); err != nil {
		t.Skip("leyline binary not on PATH")
	}

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dbPath, cleanup, err := AutoInvokeLeylineParse(src)
	require.NoError(t, err)
	require.NotEmpty(t, dbPath)
	defer cleanup()

	info, err := os.Stat(dbPath)
	require.NoError(t, err, "produced .db must exist")
	assert.Greater(t, info.Size(), int64(0), "produced .db must be non-empty")

	cleanup()
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Errorf("cleanup did not remove %s", dbPath)
	}
}
