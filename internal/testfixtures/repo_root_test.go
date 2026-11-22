package testfixtures

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RepoRoot is what the fixture tools resolve the manifest from: the
// directory holding mache's go.mod, whatever the working directory is.
func TestRepoRoot_IsTheModuleRootFromAnyDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	root, err := RepoRoot()
	require.NoError(t, err)
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	require.NoError(t, err)
	assert.Contains(t, string(mod), "module github.com/agentic-research/mache\n")
}

func TestGitShortSHA(t *testing.T) {
	assert.Empty(t, GitShortSHA(t.TempDir()), "a directory that is not a checkout has no SHA")

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "x"},
	} {
		// No hooks: a developer's global commit hook must not decide this test.
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "core.hooksPath=/dev/null"}, args...)...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
	full, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	require.NoError(t, err)
	sha := GitShortSHA(dir)
	require.NotEmpty(t, sha)
	assert.True(t, strings.HasPrefix(string(full), sha), "the short SHA prefixes HEAD")
	assert.NotContains(t, sha, "\n")
}
