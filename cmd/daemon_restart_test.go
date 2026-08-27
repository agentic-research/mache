package cmd

import (
	"bytes"
	"errors"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubDaemonSettle removes this file's LAST environment dependence.
//
// The stubs above already remove the supervisor's: without them these tests
// reached real launchctl and passed only where a daemon happened to be loaded.
// confirmRestart then added a second one — a real HTTP probe of
// localhost:7532 — so the same tests passed on a developer machine with the
// daemon up and, on CI, spent the full 20s settle window before taking the
// warning path. That is how this shipped green locally and failed on both CI
// platforms (PR #637).
//
// up=true makes the endpoint answer immediately, which is the state a restart
// is supposed to produce.
func stubDaemonSettle(t *testing.T, up bool) {
	t.Helper()
	prevProbe := daemonEndpointUp
	prevTimeout, prevPoll := daemonSettleTimeout, daemonSettlePoll
	daemonEndpointUp = func() (string, bool) {
		if up {
			return "1.2.3-test", true
		}
		return "", false
	}
	daemonSettleTimeout = 30 * time.Millisecond
	daemonSettlePoll = 5 * time.Millisecond
	t.Cleanup(func() {
		daemonEndpointUp = prevProbe
		daemonSettleTimeout, daemonSettlePoll = prevTimeout, prevPoll
	})
}

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

	stubDaemonSettle(t, true)

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
	prev, prevQuery, prevAuto := runSupervisorCmd, querySupervisorCmd, daemonAgentAutoload
	t.Cleanup(func() {
		runSupervisorCmd, querySupervisorCmd, daemonAgentAutoload = prev, prevQuery, prevAuto
	})
	daemonAgentAutoload = true

	// Stub the STATE query too. Without this the darwin path asks the real
	// launchctl, finds no loaded job on a CI runner, and returns before ever
	// reaching runSupervisorCmd — so this test passed only on a machine that
	// happened to have a daemon running. That is exactly the environment
	// dependence these stubs exist to remove.
	querySupervisorCmd = func(string, ...string) (string, error) {
		return "mache = {\n\tstate = running\n\tprogram = /x/mache\n}\n", nil
	}

	var got [][]string
	runSupervisorCmd = func(name string, args ...string) error {
		got = append(got, append([]string{filepath.Base(name)}, args...))
		return nil
	}

	stubDaemonSettle(t, true)

	var buf bytes.Buffer
	restartDaemonAgent(&buf)

	switch runtime.GOOS {
	case "darwin":
		// A RELOAD, not `kickstart -k`. launchd pins the job's code identity
		// at bootstrap, and mache's ad-hoc signature changes identity on every
		// build — so kickstarting the old registration after the binary was
		// replaced makes the kernel SIGKILL the new binary at exec
		// (CODESIGNING "Launch Constraint Violation", from a live crash
		// report; the 10s/43s/112s restart-gap family, mache-706d8f). The
		// previous assertions here REQUIRED -k and FORBADE bootstrap —
		// pinning the exact behaviour that caused the kills. The
		// restart-if-running property they meant to protect lives in the
		// !job.Running guard, covered by DoesNotStartAnIdleJob.
		require.Len(t, got, 3, "darwin restart is bootout -> bootstrap -> kickstart")
		assert.Contains(t, got[0], "bootout")
		assert.Contains(t, got[1], "bootstrap")
		assert.Contains(t, got[2], "kickstart")
	case "linux":
		require.Len(t, got, 1)
		assert.Contains(t, got[0], "try-restart", "try-restart is restart-if-running")
		assert.NotContains(t, got[0], "enable", "enable --now would START a stopped service")
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
	prev, prevQuery, prevAuto := runSupervisorCmd, querySupervisorCmd, daemonAgentAutoload
	t.Cleanup(func() {
		runSupervisorCmd, querySupervisorCmd, daemonAgentAutoload = prev, prevQuery, prevAuto
	})
	daemonAgentAutoload = true
	// Report a RUNNING job so the restart is actually attempted — otherwise on
	// a machine with no daemon this passes without reaching the failure path
	// it exists to exercise, which is a vacuous green.
	querySupervisorCmd = func(string, ...string) (string, error) {
		return "mache = {\n\tstate = running\n\tprogram = /x/mache\n}\n", nil
	}
	runSupervisorCmd = func(string, ...string) error { return errors.New("Could not find service") }

	var buf bytes.Buffer
	stubDaemonSettle(t, true)
	require.NotPanics(t, func() { restartDaemonAgent(&buf) })
	assert.NotContains(t, buf.String(), "restarted the supervised daemon",
		"must not claim a restart that did not happen")
	// Since the reload change this is no longer fully silent on darwin: a
	// bootstrap failure while a RUNNING job is being reloaded warns, because
	// the reload tore the job down and could not bring it back — silence there
	// would hide a daemon this command just removed.
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

// TestRestartDaemonAgent_DoesNotStartAnIdleJob pins the property the argv
// assertion above CANNOT reach.
//
// TestRestartDaemonAgent_UsesRestartNotStart checks that the command is
// `kickstart -k` and not `bootstrap`. That distinction turns out not to carry
// the guarantee: per man launchctl, kickstart runs a service "regardless of
// its configured launch conditions", and -k only adds kill-first WHEN ALREADY
// RUNNING. So kickstart on a loaded-but-idle job STARTS it.
//
// That state is reachable for mache — the plist sets
// KeepAlive{SuccessfulExit:false} and `mache serve` exits 0 on SIGTERM, so
// `launchctl stop` leaves the job loaded and idle. Before the state guard, the
// next `task install` resurrected a daemon the user had deliberately stopped
// and reported it as a restart.
//
// Found by an external review of PR #589; the defect was in already-merged
// #588 and no argv-shaped test could have caught it.
func TestRestartDaemonAgent_DoesNotStartAnIdleJob(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchctl kickstart semantics are darwin-specific; systemctl try-restart is restart-only by contract")
	}
	prevRun, prevQuery, prevAuto := runSupervisorCmd, querySupervisorCmd, daemonAgentAutoload
	t.Cleanup(func() {
		runSupervisorCmd, querySupervisorCmd, daemonAgentAutoload = prevRun, prevQuery, prevAuto
	})
	daemonAgentAutoload = true

	// A real `launchctl print` for a job that is loaded but NOT running.
	querySupervisorCmd = func(string, ...string) (string, error) {
		return "com.agentic-research.mache = {\n\tstate = not running\n\tprogram = /Users/x/.local/bin/mache\n}\n", nil
	}
	var ran [][]string
	runSupervisorCmd = func(name string, args ...string) error {
		ran = append(ran, append([]string{name}, args...))
		return nil
	}

	stubDaemonSettle(t, true)

	var buf bytes.Buffer
	restartDaemonAgent(&buf)

	assert.Empty(t, ran,
		"a loaded-but-IDLE job must not be kickstarted — that STARTS a daemon the user stopped")
	assert.Empty(t, buf.String(),
		"must not report a restart that did not happen")
}

// TestRestartDaemonAgent_RestartsARunningJob is the positive control: the
// guard must not be so strict it never fires.
func TestRestartDaemonAgent_RestartsARunningJob(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin-specific path")
	}
	prevRun, prevQuery, prevAuto := runSupervisorCmd, querySupervisorCmd, daemonAgentAutoload
	t.Cleanup(func() {
		runSupervisorCmd, querySupervisorCmd, daemonAgentAutoload = prevRun, prevQuery, prevAuto
	})
	daemonAgentAutoload = true

	querySupervisorCmd = func(string, ...string) (string, error) {
		return "com.agentic-research.mache = {\n\tstate = running\n\tprogram = /Users/x/.local/bin/mache\n\tpid = 123\n}\n", nil
	}
	var ran [][]string
	runSupervisorCmd = func(name string, args ...string) error {
		ran = append(ran, append([]string{name}, args...))
		return nil
	}

	stubDaemonSettle(t, true)

	var buf bytes.Buffer
	restartDaemonAgent(&buf)

	require.Len(t, ran, 3, "a RUNNING job is RELOADED: bootout, bootstrap, kickstart (mache-706d8f)")
	assert.Contains(t, ran[0], "bootout")
	assert.Contains(t, ran[1], "bootstrap")
	assert.Contains(t, ran[2], "kickstart")
	assert.Contains(t, buf.String(), "restarted the supervised daemon")
}

// TestParseLaunchctlPrint_DecodesPathsThePlistWriterEscapes closes the reader/
// writer asymmetry an external review found: supervisorBinary (PR #589) read
// the plist directly and never reversed xmlText's escaping, so a path with a
// space, `&` or a quote produced a wrong answer and a hard test failure for
// exactly the user xmlText was written for. launchctl reports `program =`
// already decoded, so no unescaping is needed — pinned here so nobody
// "simplifies" this back to parsing the plist.
func TestParseLaunchctlPrint_DecodesPathsThePlistWriterEscapes(t *testing.T) {
	for _, path := range []string{
		"/Applications/Mache Tools/mache", // the case xmlText's doc comment names
		"/opt/AT&T/mache",
		"/home/u/it's mine/mache",
		`/home/u/we"ird/mache`,
	} {
		t.Run(path, func(t *testing.T) {
			job := parseLaunchctlPrint("mache = {\n\tstate = running\n\tprogram = " + path + "\n}\n")
			assert.Equal(t, path, job.Program, "launchctl reports the raw path; no unescaping should be applied")
			assert.True(t, job.Running)
		})
	}
}
