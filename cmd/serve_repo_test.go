package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// initTestGitRepo creates a temp git repo with one commit for worktree tests.
func initTestGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, cmd := range [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
	} {
		c := exec.Command(cmd[0], cmd[1:]...)
		c.Dir = dir
		require.NoError(t, c.Run(), "git init failed")
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644))
	for _, cmd := range [][]string{
		{"git", "add", "."},
		{"git", "commit", "-m", "init"},
	} {
		c := exec.Command(cmd[0], cmd[1:]...)
		c.Dir = dir
		require.NoError(t, c.Run())
	}
	return dir
}

// ---------------------------------------------------------------------------
// createWorktree / removeWorktree
// ---------------------------------------------------------------------------

func TestCreateWorktree(t *testing.T) {
	cloneDir := initTestGitRepo(t)

	wtDir, err := createWorktree(cloneDir, "session-1")
	require.NoError(t, err)
	defer func() { _ = removeWorktree(cloneDir, wtDir) }()

	// Worktree directory should exist and contain the file from the commit.
	info, err := os.Stat(wtDir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())

	_, err = os.Stat(filepath.Join(wtDir, "main.go"))
	assert.NoError(t, err, "worktree should contain committed files")
}

func TestCreateWorktree_UniquePerSession(t *testing.T) {
	cloneDir := initTestGitRepo(t)

	wt1, err := createWorktree(cloneDir, "session-a")
	require.NoError(t, err)
	defer func() { _ = removeWorktree(cloneDir, wt1) }()

	wt2, err := createWorktree(cloneDir, "session-b")
	require.NoError(t, err)
	defer func() { _ = removeWorktree(cloneDir, wt2) }()

	assert.NotEqual(t, wt1, wt2, "each session should get a unique worktree")
}

func TestRemoveWorktree(t *testing.T) {
	cloneDir := initTestGitRepo(t)

	wtDir, err := createWorktree(cloneDir, "session-rm")
	require.NoError(t, err)

	err = removeWorktree(cloneDir, wtDir)
	require.NoError(t, err)

	_, err = os.Stat(wtDir)
	assert.True(t, os.IsNotExist(err), "worktree dir should be removed")
}

func TestCreateWorktree_InvalidDir(t *testing.T) {
	invalidDir := t.TempDir() // exists but is not a git repo
	_, err := createWorktree(invalidDir, "session-x")
	assert.Error(t, err, "should fail on non-git directory")
}

func TestSanitizeSessionID(t *testing.T) {
	// Safe IDs
	safe, err := sanitizeSessionID("abc-123_def")
	assert.NoError(t, err)
	assert.Equal(t, "abc-123_def", safe)

	// Unsafe: path separator
	_, err = sanitizeSessionID("../../etc/passwd")
	assert.Error(t, err)

	_, err = sanitizeSessionID("session/evil")
	assert.Error(t, err)

	// Unsafe: empty
	_, err = sanitizeSessionID("")
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// graphRegistry repo mode integration
// ---------------------------------------------------------------------------

func TestRepoHTTP_SessionIsolation(t *testing.T) {
	cloneDir := initTestGitRepo(t)

	r := newGraphRegistry("", nil)
	r.repoCloneDir = cloneDir

	// Simulate two sessions resolving their graphs
	path1 := r.repoWorktreePath("session-1")
	path2 := r.repoWorktreePath("session-2")

	assert.NotEqual(t, path1, path2, "different sessions should get different paths")
	assert.Contains(t, path1, "session-1")
	assert.Contains(t, path2, "session-2")
}

func TestRepoHTTP_CleanupOnDisconnect(t *testing.T) {
	cloneDir := initTestGitRepo(t)

	r := newGraphRegistry("", nil)
	r.repoCloneDir = cloneDir

	// Create a worktree for a session
	wtDir, err := createWorktree(cloneDir, "session-cleanup")
	require.NoError(t, err)
	r.worktrees.Store("session-cleanup", wtDir)

	// Verify it exists
	_, err = os.Stat(wtDir)
	require.NoError(t, err)

	// Simulate disconnect cleanup
	r.cleanupRepoSession("session-cleanup")

	// Worktree should be gone
	_, err = os.Stat(wtDir)
	assert.True(t, os.IsNotExist(err), "worktree should be cleaned up on disconnect")

	// Map entry should be gone
	_, loaded := r.worktrees.Load("session-cleanup")
	assert.False(t, loaded, "worktree map entry should be removed")
}

func TestRepoStdio_NoWorktree(t *testing.T) {
	// In stdio mode, repoCloneDir is empty — no worktree creation
	r := newGraphRegistry("/some/path", nil)
	assert.Empty(t, r.repoCloneDir, "non-repo mode should have empty repoCloneDir")
}
