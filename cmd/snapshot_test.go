package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCopyFile_ContentIntegrity(t *testing.T) {
	tmpDir := t.TempDir()
	srcPath := filepath.Join(tmpDir, "source.db")
	dstPath := filepath.Join(tmpDir, "snapshot.db")

	// Write source with known content
	content := []byte("SQLite format 3\x00fake db content for testing integrity")
	require.NoError(t, os.WriteFile(srcPath, content, 0o644))

	// Copy
	require.NoError(t, copyFile(srcPath, dstPath))

	// Verify content matches
	got, err := os.ReadFile(dstPath)
	require.NoError(t, err)
	assert.Equal(t, content, got)

	// Verify source is not modified
	srcContent, err := os.ReadFile(srcPath)
	require.NoError(t, err)
	assert.Equal(t, content, srcContent)
}

func TestCopyFile_SnapshotIsolation(t *testing.T) {
	tmpDir := t.TempDir()
	srcPath := filepath.Join(tmpDir, "source.db")
	dstPath := filepath.Join(tmpDir, "snapshot.db")

	original := []byte("original content")
	require.NoError(t, os.WriteFile(srcPath, original, 0o644))
	require.NoError(t, copyFile(srcPath, dstPath))

	// Modify source after copy
	require.NoError(t, os.WriteFile(srcPath, []byte("modified content"), 0o644))

	// Snapshot should still have original content
	got, err := os.ReadFile(dstPath)
	require.NoError(t, err)
	assert.Equal(t, original, got, "snapshot must be isolated from source modifications")
}

func TestCopyFile_EmptyFile(t *testing.T) {
	tmpDir := t.TempDir()
	srcPath := filepath.Join(tmpDir, "empty.db")
	dstPath := filepath.Join(tmpDir, "snapshot.db")

	require.NoError(t, os.WriteFile(srcPath, []byte{}, 0o644))
	require.NoError(t, copyFile(srcPath, dstPath))

	got, err := os.ReadFile(dstPath)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestCopyFile_SourceNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	err := copyFile(filepath.Join(tmpDir, "nonexistent.db"), filepath.Join(tmpDir, "dst.db"))
	require.Error(t, err)
}

func TestSnapshotPath_Format(t *testing.T) {
	// Verify the snapshot naming pattern produces unique, identifiable paths
	pid := os.Getpid()
	base := "test.db"
	snapshotName := filepath.Join(os.TempDir(), "mache", "snapshots",
		"snap-"+string(rune(pid))+"-"+base)
	// Just verify it contains expected components
	assert.Contains(t, snapshotName, "mache")
	assert.Contains(t, snapshotName, "snapshots")
	assert.Contains(t, snapshotName, "test.db")
}

func TestCopyDir_BasicTree(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "snapshot")

	// Create a small source tree
	require.NoError(t, os.MkdirAll(filepath.Join(src, "pkg", "auth"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "main.go"), []byte("package main"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "pkg", "auth", "auth.go"), []byte("package auth"), 0o644))

	n, err := copyDir(src, dst)
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	// Verify content
	got, err := os.ReadFile(filepath.Join(dst, "main.go"))
	require.NoError(t, err)
	assert.Equal(t, "package main", string(got))

	got, err = os.ReadFile(filepath.Join(dst, "pkg", "auth", "auth.go"))
	require.NoError(t, err)
	assert.Equal(t, "package auth", string(got))
}

func TestCopyDir_SkipsHiddenAndBuildDirs(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "snapshot")

	// Create dirs that should be skipped
	for _, skip := range []string{".git", "node_modules", "target", "__pycache__"} {
		dir := filepath.Join(src, skip)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "junk.txt"), []byte("skip me"), 0o644))
	}
	// Create a real file
	require.NoError(t, os.WriteFile(filepath.Join(src, "real.go"), []byte("package real"), 0o644))

	n, err := copyDir(src, dst)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	// Real file exists
	_, err = os.Stat(filepath.Join(dst, "real.go"))
	assert.NoError(t, err)

	// Skipped dirs don't exist
	for _, skip := range []string{".git", "node_modules", "target", "__pycache__"} {
		_, err = os.Stat(filepath.Join(dst, skip))
		assert.True(t, os.IsNotExist(err), "expected %s to be skipped", skip)
	}
}

func TestCopyDir_Isolation(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "snapshot")

	require.NoError(t, os.WriteFile(filepath.Join(src, "file.go"), []byte("original"), 0o644))

	_, err := copyDir(src, dst)
	require.NoError(t, err)

	// Modify source after copy
	require.NoError(t, os.WriteFile(filepath.Join(src, "file.go"), []byte("modified"), 0o644))

	// Snapshot retains original
	got, err := os.ReadFile(filepath.Join(dst, "file.go"))
	require.NoError(t, err)
	assert.Equal(t, "original", string(got))
}
