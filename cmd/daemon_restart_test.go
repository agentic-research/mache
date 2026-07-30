package cmd

import (
	"bytes"
	"errors"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The defect these pin (mache install / task install leaving a stale daemon):
// replacing the binary on disk does not re-exec a supervisor that is already
// running. Measured on a real machine — installed binary v0.20.0, daemon on
// :7532 still answering as 0.19.0-10-g1c3d812, every MCP session served
// pre-release code with nothing reporting the mismatch.

// TestRestartDaemonAgent_RespectsAutoloadGate pins that the autoload gate
// suppresses the restart entirely. Tests and any non-interactive path set it
// false; without this check a `go test` run would kickstart the developer's
// real daemon as a side effect.
func TestRestartDaemonAgent_RespectsAutoloadGate(t *testing.T) {
	prev := daemonAgentAutoload
	daemonAgentAutoload = false
	t.Cleanup(func() { daemonAgentAutoload = prev })

	var buf bytes.Buffer
	restartDaemonAgent(&buf)
	assert.Empty(t, buf.String(),
		"with autoload gated off the restart must be a total no-op — no supervisor call, no output")
}

// TestRestartDaemonAgent_UsesRestartNotStart is the load-bearing assertion.
// The command MUST be a restart-if-running form. `launchctl bootstrap` /
// `systemctl --user enable --now` would START a daemon on a machine whose
// owner never ran `mache init --global` — installing a binary must not conjure
// a background process. Asserting the argv is the only way to check this;
// running it would both hide the distinction and restart the developer's own
// daemon.
func TestRestartDaemonAgent_UsesRestartNotStart(t *testing.T) {
	prev, prevAuto := runSupervisorCmd, daemonAgentAutoload
	t.Cleanup(func() { runSupervisorCmd, daemonAgentAutoload = prev, prevAuto })
	daemonAgentAutoload = true

	var got []string
	runSupervisorCmd = func(name string, args ...string) error {
		got = append([]string{filepath.Base(name)}, args...)
		return nil
	}

	var buf bytes.Buffer
	restartDaemonAgent(&buf)

	switch runtime.GOOS {
	case "darwin":
		require.NotEmpty(t, got, "darwin must attempt a supervisor call")
		assert.Equal(t, "launchctl", got[0])
		assert.Contains(t, got, "kickstart", "must kickstart an existing job")
		assert.Contains(t, got, "-k", "-k kills the running job first; without it the old process survives")
		assert.NotContains(t, got, "bootstrap", "bootstrap would START a daemon the user did not ask for")
	case "linux":
		require.NotEmpty(t, got, "linux must attempt a supervisor call")
		assert.Equal(t, "systemctl", got[0])
		assert.Contains(t, got, "try-restart", "try-restart is restart-if-running")
		assert.NotContains(t, got, "enable", "enable --now would START a stopped service")
		assert.NotContains(t, got, "start")
	default:
		assert.Empty(t, got, "unsupported platforms must not shell out")
	}
	if len(got) > 0 {
		assert.Contains(t, buf.String(), "restarted the supervised daemon")
	}
}

// TestRestartDaemonAgent_SupervisorFailureIsSilent pins that a supervisor
// error (overwhelmingly: the label is not loaded, because the user never ran
// `mache init --global`) is NOT announced. Reporting it would tell a user
// something is wrong when nothing is — and claiming a restart that did not
// happen is worse, since it implies the daemon is current.
func TestRestartDaemonAgent_SupervisorFailureIsSilent(t *testing.T) {
	prev, prevAuto := runSupervisorCmd, daemonAgentAutoload
	t.Cleanup(func() { runSupervisorCmd, daemonAgentAutoload = prev, prevAuto })
	daemonAgentAutoload = true
	runSupervisorCmd = func(string, ...string) error { return errors.New("Could not find service") }

	var buf bytes.Buffer
	require.NotPanics(t, func() { restartDaemonAgent(&buf) })
	assert.Empty(t, buf.String(),
		"a not-loaded supervisor is the benign common case; it must not claim a restart nor report an error")
}

// TestDaemonRestartCmd_Wired pins that `mache daemon restart` exists, takes no
// args, and is reachable from the root command. The Taskfile's install target
// invokes exactly this string, so a rename here silently breaks `task install`
// — the docs/CLI gate (mache-e7c825) covers documented invocations, and this
// covers the Taskfile's.
func TestDaemonRestartCmd_Wired(t *testing.T) {
	c, _, err := rootCmd.Find([]string{"daemon", "restart"})
	require.NoError(t, err)
	require.NotNil(t, c)
	assert.Equal(t, "restart", c.Name())
	assert.NotNil(t, c.Args, "must reject stray args rather than ignoring them")
	require.NoError(t, c.Args(c, nil), "zero args is the supported form")
	assert.Error(t, c.Args(c, []string{"unexpected"}),
		"an unknown arg must fail loudly, not be silently ignored")
}
