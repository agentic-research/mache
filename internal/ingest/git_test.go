package ingest

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadGitCommits(t *testing.T) {
	// Create temp dir
	tmpDir := t.TempDir()

	// Init git repo
	runGit(t, tmpDir, "init")
	runGit(t, tmpDir, "config", "user.name", "Tester")
	runGit(t, tmpDir, "config", "user.email", "test@example.com")

	// Commit 1
	runGit(t, tmpDir, "commit", "--allow-empty", "-m", "Initial commit")

	// Commit 2
	runGit(t, tmpDir, "commit", "--allow-empty", "-m", `Second commit

With body`)

	// Load
	commits, err := LoadGitCommits(tmpDir)
	require.NoError(t, err)
	assert.Len(t, commits, 2)

	// Check content (most recent first usually)
	c0 := commits[0].(map[string]any)
	assert.Equal(t, "Tester", c0["author"])
	assert.Contains(t, c0["message"], "Second commit")
	assert.Contains(t, c0["message"], "With body")
	assert.NotEmpty(t, c0["sha"])
	assert.NotEmpty(t, c0["date"])

	c1 := commits[1].(map[string]any)
	assert.Contains(t, c1["message"], "Initial commit")
}

func runGit(t *testing.T, dir string, args ...string) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	err := cmd.Run()
	require.NoError(t, err, "git %v failed", args)
}
