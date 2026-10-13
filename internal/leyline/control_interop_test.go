package leyline

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/control"
)

// A control block written by ley-line-open itself opens in mache and reads
// back the arena it names (mache-3451a1). mache keeps its own Go copy of the
// control block, so the two implementations agreeing on the layout and the
// v3 seqlock is something only the real binary can show. Runs when a leyline
// that writes v3 (v0.20.1 or later) is on PATH, as it is in CI on the pin.
func TestControlBlock_ReadsWhatLeylineWrites(t *testing.T) {
	bin, err := exec.LookPath("leyline")
	if err != nil {
		t.Skip("leyline not on PATH")
	}
	out, err := exec.Command(bin, "--version").Output()
	require.NoError(t, err)
	fields := strings.Fields(string(out)) // "leyline 0.20.3 (open)"
	require.GreaterOrEqual(t, len(fields), 2, "unexpected --version output %q", out)
	if compareSemver(fields[1], "0.20.1") < 0 {
		t.Skipf("leyline %s writes control block v2; v3 arrived in 0.20.1", fields[1])
	}

	// A short directory: the daemon's socket goes beside the control block,
	// and a t.TempDir path can push it past the 104-byte limit macOS puts on
	// a socket path.
	dir, err := os.MkdirTemp("", "llctl")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	src := filepath.Join(dir, "src")
	require.NoError(t, os.MkdirAll(src, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "a.go"), []byte("package a\n\nfunc A() {}\n"), 0o644))
	db := filepath.Join(dir, "a.db")
	ctrl := filepath.Join(dir, "a.ctrl")
	runOK := func(args ...string) {
		t.Helper()
		out, err := exec.Command(bin, args...).CombinedOutput()
		require.NoError(t, err, "leyline %s: %s", strings.Join(args, " "), out)
	}
	runOK("parse", src, "-o", db)

	// The arena is created by a daemon. Headless (no --mount), on a private
	// arena and control block, stopped at the end of the test.
	arena := filepath.Join(dir, "a.arena")
	daemon := exec.Command(bin, "daemon", "--arena", arena, "--arena-size-mib", "8", "--control", ctrl)
	setProcessGroup(daemon) // a daemon may spawn children; stop them with it
	require.NoError(t, daemon.Start())
	waitErr := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		waitErr <- daemon.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = signalProcessGroup(daemon.Process, syscall.SIGTERM)
		<-exited
	})
	// The daemon's socket sits beside its control block, same stem.
	sock := strings.TrimSuffix(ctrl, ".ctrl") + ".sock"
	ready, gone, werr := awaitDaemonSocket(sock, waitErr, leylineStartTimeout())
	require.True(t, ready, "daemon socket never appeared (exited=%v: %v)", gone, werr)

	runOK("load", "--db", db, "--control", ctrl)

	c, err := control.OpenOrCreate(ctrl)
	require.NoError(t, err, "mache must open the control block leyline wrote")
	defer func() { _ = c.Close() }()

	// leyline records the canonical path (on macOS the temp dir is under a
	// /var -> /private/var symlink).
	want, err := filepath.EvalSymlinks(arena)
	require.NoError(t, err)
	require.Equal(t, want, c.GetArenaPath(), "the control block names the daemon's arena")
	info, err := os.Stat(arena)
	require.NoError(t, err, "the arena the control block names exists")
	assert.Equal(t, uint64(info.Size()), c.GetArenaSize())
	assert.False(t, control.IsZeroRoot(c.GetCurrentRoot()), "a loaded arena has a published root")
}
