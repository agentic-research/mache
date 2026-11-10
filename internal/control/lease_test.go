package control_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/agentic-research/mache/internal/control"
	"github.com/agentic-research/mache/internal/lltest"
)

// tryExclusive reports whether an exclusive flock on path can be taken right
// now, as leyline's --reset-arena tries it.
func tryExclusive(t *testing.T, path string) bool {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return false
	}
	require.NoError(t, err)
	return true
}

// A Controller holds leyline's shared lease, <ctrl>.lock, while it maps the
// control block, and drops it on Close (mache-6b6a90).
func TestController_HoldsTheLeaseWhileOpen(t *testing.T) {
	ctrl := filepath.Join(t.TempDir(), "a.ctrl")
	c, err := control.OpenOrCreate(ctrl)
	require.NoError(t, err)
	assert.False(t, tryExclusive(t, ctrl+".lock"), "the lease was not held while the control block was open")
	require.NoError(t, c.Close())
	assert.True(t, tryExclusive(t, ctrl+".lock"), "Close did not drop the lease")
}

// With a mache Controller open on a control block, the pinned leyline refuses
// to --reset-arena it: a reset replaces the control file, and the mapping
// would go on reading the old one without ever seeing another publish.
func TestController_LeylineRefusesToResetUnderAnOpenController(t *testing.T) {
	ll := lltest.ResolveBinaryOrSkip(t)
	// Under the temp dir, where OpenOrCreate admits a control file; the
	// directory name is short, as macOS caps socket paths at 104 bytes.
	dir, err := os.MkdirTemp("", "llls")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ctrl := filepath.Join(dir, "a.ctrl")

	c, err := control.OpenOrCreate(ctrl)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	// A refusal exits at once. A daemon still running at the deadline took
	// the reset and is serving, which is the failure; it is killed so the
	// test ends instead of waiting on it.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ll.Path, "daemon", "--arena", filepath.Join(dir, "a.arena"),
		"--arena-size-mib", "8", "--control", ctrl, "--reset-arena")
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	require.NoError(t, ctx.Err(), "leyline reset a control block a mache Controller was mapping, and served:\n%s", out)
	require.Error(t, err, "leyline reset a control block a mache Controller was mapping:\n%s", out)
	assert.Contains(t, string(out), "--reset-arena refused: another process holds")
}
