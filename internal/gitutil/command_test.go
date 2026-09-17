package gitutil

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHermeticGitCommand(t *testing.T) {
	t.Setenv("GIT_DIR", "/tmp/poison.git")

	cmd := HermeticGitCommand("--version")
	assert.NotContains(t, cmd.Env, "GIT_DIR=/tmp/poison.git")
	require.NoError(t, cmd.Run())
}

func TestWithoutLocalEnv(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		"GIT_DIR=/repo/.git",
		"GIT_INDEX_FILE=/repo/.git/index",
		"GIT_CONFIG_COUNT=2",
		"HOME=/tmp/home",
	}

	assert.Equal(t, []string{"PATH=/usr/bin", "HOME=/tmp/home"}, WithoutLocalEnv(env))
}

// TestHermeticGitCommand_IgnoresGlobalHooks pins the reason this constructor
// exists at all: a developer's global Git configuration must not reach into
// mache's own temp repositories.
//
// rsry installs a global core.hooksPath whose commit-msg hook rejects any
// message without a bead reference. It fired inside t.TempDir() repositories
// and failed seven tests in internal/mcpserve on `git commit -m "init"` — for
// a machine-local reason CI never sees, because the runner has no such hook.
//
// The test installs exactly that shape of hook, so it fails without the fix on
// ANY machine rather than only on one that happens to have rsry's.
func TestHermeticGitCommand_IgnoresGlobalHooks(t *testing.T) {
	hooks := t.TempDir()
	hook := filepath.Join(hooks, "commit-msg")
	require.NoError(t, os.WriteFile(hook,
		[]byte("#!/bin/sh\necho 'commit must start with a bead reference' >&2\nexit 1\n"), 0o755))

	// A repository whose config points at that hook directory, which is what a
	// global core.hooksPath looks like from git's point of view.
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "."},
		{"config", "core.hooksPath", hooks},
		{"config", "user.name", "t"},
		{"config", "user.email", "t@example.com"},
	} {
		c := HermeticGitCommand(args...)
		c.Dir = repo
		require.NoError(t, c.Run(), "setup: git %v", args)
	}
	require.NoError(t, os.WriteFile(filepath.Join(repo, "a.txt"), []byte("x\n"), 0o644))

	add := HermeticGitCommand("add", ".")
	add.Dir = repo
	require.NoError(t, add.Run())

	commit := HermeticGitCommand("commit", "-m", "init")
	commit.Dir = repo
	out, err := commit.CombinedOutput()
	require.NoError(t, err,
		"a hook from the developer's configuration ran inside a test repository: %s", out)
}
