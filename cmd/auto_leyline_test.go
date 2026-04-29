package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAutoInvokeLeylineParse_NoBinary verifies the helper returns a clear
// error when leyline is not on PATH and not in the bundled location.
func TestAutoInvokeLeylineParse_NoBinary(t *testing.T) {
	// Hide both PATH and the bundled location for this test
	t.Setenv("PATH", "")
	t.Setenv("HOME", t.TempDir())

	_, _, err := autoInvokeLeylineParse(t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "leyline")
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

	dbPath, cleanup, err := autoInvokeLeylineParse(src)
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
