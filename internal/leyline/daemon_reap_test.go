package leyline

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The daemon a test starts through DiscoverOrStart under an e2eHome is gone
// when that test ends, and it went gracefully: it was stopped BEFORE its home
// was removed. Stopped after, the v0.29.0 daemon's final snapshot blocks 30s
// in a pool checkout on the unlinked live db (measured: 30.02s, against
// 0.04-0.48s with the directory intact), mache gives up after 3s and
// SIGKILLs, and a test binary that died first left it as an orphan under
// /tmp/leyline-disc-* (mache-252d27). The wall-clock bound below is loose
// against a graceful stop and tight against the 3s SIGKILL budget, so the
// ordering regression is what it catches. Removing e2eHome's StopManaged
// cleanup fails the liveness assertion.
func TestE2EHome_StopsTheManagedDaemonBeforeRemovingItsHome(t *testing.T) {
	bin, err := ResolveBinary(false)
	if err != nil {
		t.Skipf("pinned leyline unavailable: %v", err)
	}
	var (
		pid                int
		cleanupBegan, over time.Time
	)
	t.Run("a test that starts a daemon", func(t *testing.T) {
		resetManaged(t)
		// Registered first, so it runs last: the moment every cleanup is done.
		t.Cleanup(func() { over = time.Now() })
		home := e2eHome(t)
		dot := filepath.Join(home, ".mache")
		require.NoError(t, os.MkdirAll(filepath.Join(dot, "bin"), 0o755))
		require.NoError(t, os.Symlink(bin, filepath.Join(dot, "bin", "leyline-"+leylineBinaryVersion)))
		t.Setenv("HOME", home)
		t.Setenv("LEYLINE_SOCKET", "")
		t.Setenv("PATH", "/nonexistent-path-for-test")
		t.Setenv("MACHE_NO_LEYLINE", "1")

		_, err := DiscoverOrStart()
		require.NoError(t, err)
		managed.mu.Lock()
		pid = managed.proc.Pid
		managed.mu.Unlock()
		// Registered last, so it runs first: the moment cleanup begins.
		t.Cleanup(func() { cleanupBegan = time.Now() })
	})
	require.NotZero(t, pid)
	assert.ErrorIs(t, syscall.Kill(pid, 0), syscall.ESRCH,
		"daemon pid %d is still alive after the test that started it ended", pid)
	assert.Less(t, over.Sub(cleanupBegan), 2*time.Second,
		"the cleanup took as long as the SIGTERM budget: the daemon was stopped after its home was removed, not before")
}
