package leyline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startFailingDaemon runs DiscoverOrStart against a pinned-version stub whose
// daemon invocation runs body, and returns the error and how long it took.
func startFailingDaemon(t *testing.T, body string) (time.Duration, error) {
	t.Helper()
	resetManaged(t)
	t.Cleanup(func() { resetManaged(t) })
	home := e2eHome(t)
	binDir := filepath.Join(home, ".mache", "bin")
	require.NoError(t, os.MkdirAll(binDir, 0o755))
	pin := strings.TrimPrefix(leylineBinaryVersion, "v")
	fake := "#!/bin/sh\ncase \"$1\" in --version) echo 'leyline " + pin + " (open)'; exit 0;; esac\n" + body + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "leyline"), []byte(fake), 0o755))
	t.Setenv("HOME", home)
	t.Setenv("LEYLINE_SOCKET", "")
	t.Setenv("PATH", "/nonexistent-path-for-test")
	t.Setenv("MACHE_NO_LEYLINE", "1")
	t.Setenv("MACHE_LEYLINE_START_TIMEOUT", "30s")
	start := time.Now()
	_, err := DiscoverOrStart()
	return time.Since(start), err
}

// A daemon that dies at startup is reported with what it said. The message is
// the one leyline v0.27.0 printed on a machine whose ~/.mache/default.ctrl
// predated control block v3, where mache reported only "exit status 1" on
// every session (mache-e70109).
func TestDiscoverOrStart_StartupFailureCarriesTheDaemonsStderr(t *testing.T) {
	_, err := startFailingDaemon(t, `printf 'Error: open controller\n\nCaused by:\n    control block VERSION mismatch: file has v2, this binary expects v3\n' >&2
exit 1`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exited during startup")
	assert.Contains(t, err.Error(), "control block VERSION mismatch: file has v2, this binary expects v3")
}

// The daemon's children inherit its stderr pipe (the daemon spawns `mache
// serve --control`). One still holding it after the daemon exits must not hold
// up the report until that child exits too.
func TestDiscoverOrStart_StartupFailureIsNotHeldByAChildOnTheStderrPipe(t *testing.T) {
	took, err := startFailingDaemon(t, `/bin/sleep 30 &
echo 'Error: arena locked' >&2
exit 1`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Error: arena locked")
	assert.Less(t, took, 15*time.Second, "the report waited for a child holding the stderr pipe")
}

func TestStderrTail_KeepsTheLastBytesOnly(t *testing.T) {
	tail := newStderrTail(16)
	_, _ = tail.Write([]byte("0123456789"))
	_, _ = tail.Write([]byte("abcdefghij"))
	assert.Equal(t, "456789abcdefghij", tail.String())

	small := newStderrTail(64)
	_, _ = small.Write([]byte("  short\n"))
	assert.Equal(t, "short", small.String())
}

// After an upgrade, ~/.mache/default.ctrl can still hold a control block an
// older leyline wrote (v2), which the pinned leyline refuses. The upgrade also
// changes the recorded arena config, so mache passes --reset-arena, and from
// leyline v0.28.0 that rebuilds the control block when nothing else holds it
// (ley-line-open #551). The managed daemon must come up. Against the pinned
// binary; on a pin without the recovery it fails with leyline's own reason.
func TestDiscoverOrStart_RecoversAControlBlockAnOlderLeylineLeft(t *testing.T) {
	bin, err := ResolveBinary(false) // before HOME isolation, as the e2e tests do
	if err != nil {
		t.Skipf("pinned leyline unavailable: %v", err)
	}
	resetManaged(t)
	t.Cleanup(func() { StopManaged(); resetManaged(t) })
	home := e2eHome(t)
	dot := filepath.Join(home, ".mache")
	require.NoError(t, os.MkdirAll(filepath.Join(dot, "bin"), 0o755))
	require.NoError(t, os.Symlink(bin, filepath.Join(dot, "bin", "leyline-"+leylineBinaryVersion)))
	t.Setenv("HOME", home)
	t.Setenv("LEYLINE_SOCKET", "")
	t.Setenv("PATH", "/nonexistent-path-for-test")
	t.Setenv("MACHE_NO_LEYLINE", "1")

	// The control block as leyline before v3 left it: magic 'LEYC', version 2.
	ctrl := make([]byte, 4096)
	copy(ctrl, []byte{'C', 'Y', 'E', 'L', 2, 0, 0, 0})
	require.NoError(t, os.WriteFile(filepath.Join(dot, "default.ctrl"), ctrl, 0o644))
	// The arena config the previous pin recorded.
	require.NoError(t, recordArenaConfig(filepath.Join(dot, "default.arena"), arenaSpawnConfig{Leyline: "v0.26.0"}))

	sock, err := DiscoverOrStart()
	require.NoError(t, err, "the managed daemon did not start over an old control block")
	assert.True(t, isSocketAlive(sock))
}
