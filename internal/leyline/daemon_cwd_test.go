package leyline

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The managed daemon runs in its own state directory, not in whatever
// directory mache was started from. leyline v0.29.0/v0.30.0 can write an arena
// grow file at a path relative to its working directory
// (ley-line-open-f68a3a, fixed in v0.31.0), which put a 2 MiB arena at
// internal/mcpserve/.grow under `go test` and would put one in the user's repo
// in production (mache-173446). A daemon's working directory is its state
// directory regardless of that bug.
func TestSpawn_DaemonRunsInItsStateDir(t *testing.T) {
	home := fakeDaemonEnv(t, "bind")
	_, err := DiscoverOrStart()
	require.NoError(t, err)
	managedPID(t)

	stateDir, err := filepath.EvalSymlinks(filepath.Join(home, ".mache"))
	require.NoError(t, err)
	var cwd string
	require.Eventually(t, func() bool {
		b, err := os.ReadFile(filepath.Join(stateDir, "fake-cwd"))
		cwd = string(b)
		return err == nil && cwd != ""
	}, 10*time.Second, 10*time.Millisecond)
	got, err := filepath.EvalSymlinks(cwd)
	require.NoError(t, err)
	assert.Equal(t, stateDir, got, "the daemon must run in its state dir, not in the caller's working directory")
}
