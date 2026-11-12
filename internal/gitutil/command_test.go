package gitutil

import (
	"os"
	"path/filepath"
	"strings"
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

// TestIsolatedGitCommand pins the three switches that make a history read a
// function of the repository alone, and that they WIN over the developer's
// shell: a GIT_CONFIG_GLOBAL pointing at a real file is overridden, and
// GIT_NO_REPLACE_OBJECTS — which WithoutLocalEnv strips as a local variable
// — is set afresh rather than lost.
func TestIsolatedGitCommand(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/tmp/poison.gitconfig")
	t.Setenv("GIT_NO_REPLACE_OBJECTS", "")

	cmd := IsolatedGitCommand("--version")
	// exec.Cmd takes the LAST value of a duplicated key, so that is the one
	// git will see.
	assert.Equal(t, os.DevNull, lastEnv(cmd.Env, "GIT_CONFIG_GLOBAL"))
	assert.Equal(t, "1", lastEnv(cmd.Env, "GIT_CONFIG_NOSYSTEM"))
	assert.Equal(t, "1", lastEnv(cmd.Env, "GIT_NO_REPLACE_OBJECTS"))
	// Hooks stay off: it is HermeticGitCommand plus the three, not instead.
	assert.Equal(t, []string{"git", "-c", "core.hooksPath=" + os.DevNull, "--version"}, cmd.Args)
	require.NoError(t, cmd.Run())
}

// lastEnv is the value exec.Cmd uses for name: the last entry that sets it.
func lastEnv(env []string, name string) string {
	value, found := "", false
	for _, entry := range env {
		if v, ok := strings.CutPrefix(entry, name+"="); ok {
			value, found = v, true
		}
	}
	if !found {
		return "<unset>"
	}
	return value
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
