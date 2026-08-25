package cmd

import (
	"bytes"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubEndpoint replaces the liveness probe with a scripted sequence of
// answers, so verify logic can be driven without a live daemon.
func stubEndpoint(t *testing.T, answers ...bool) *int {
	t.Helper()
	prev := daemonEndpointUp
	calls := 0
	daemonEndpointUp = func() (string, bool) {
		up := false
		if calls < len(answers) {
			up = answers[calls]
		} else if len(answers) > 0 {
			up = answers[len(answers)-1] // hold the last answer
		}
		calls++
		if up {
			return "1.2.3-test", true
		}
		return "", false
	}
	t.Cleanup(func() { daemonEndpointUp = prev })
	return &calls
}

// stubSupervisor records the argv a verb would run and reports success.
func stubSupervisor(t *testing.T) *[]string {
	t.Helper()
	prev := runSupervisorCmd
	var ran []string
	runSupervisorCmd = func(name string, args ...string) error {
		ran = append(ran, name+" "+strings.Join(args, " "))
		return nil
	}
	t.Cleanup(func() { runSupervisorCmd = prev })
	return &ran
}

// TestRunDaemonVerb_StartFailsWhenNothingAnswers is the defect this whole
// surface exists to end (mache-609a10): the supervisor accepting a request is
// not the daemon serving. `launchctl kickstart` exits 0 for a job that never
// comes up, and the old code reported that as success — observed after a real
// `task install`, where the message printed while launchctl showed no PID and
// LastExitStatus 9.
func TestRunDaemonVerb_StartFailsWhenNothingAnswers(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("supervisor verbs are Unix-only")
	}
	stubSupervisor(t)
	stubEndpoint(t, false) // never comes up
	shrinkSettle(t)

	var buf bytes.Buffer
	err := runDaemonVerb(&buf, verbStart)

	require.Error(t, err, "a start that never answers must FAIL, not print success")
	assert.Contains(t, err.Error(), "nothing is answering",
		"the error must say what was checked, not just that a command ran")
	assert.NotContains(t, buf.String(), "started",
		"nothing may claim the daemon started")
}

// TestRunDaemonVerb_StartSucceedsOnlyAfterTheEndpointAnswers pins the positive
// half: success is reported on OBSERVED liveness, and names the version served
// so a stale-supervisor restart is visible rather than inferred.
func TestRunDaemonVerb_StartSucceedsOnlyAfterTheEndpointAnswers(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("supervisor verbs are Unix-only")
	}
	ran := stubSupervisor(t)
	stubEndpoint(t, false, false, true) // settles on the third poll

	var buf bytes.Buffer
	require.NoError(t, runDaemonVerb(&buf, verbStart))

	assert.Contains(t, buf.String(), "1.2.3-test", "must report the version actually served")
	require.Len(t, *ran, 1, "exactly one supervisor command")
	assert.NotContains(t, (*ran)[0], "bootout",
		"start must never unload the job; a later start would then fail to find it")
}

// TestRunDaemonVerb_StopUsesSigtermNotBootout pins the choice that makes a stop
// stick without breaking a later start. mache's plist declares
// KeepAlive{SuccessfulExit:false}, so launchd resurrects only on a NON-zero
// exit and serve handles SIGTERM cleanly — SIGTERM is therefore sufficient,
// while bootout would UNLOAD the job.
func TestRunDaemonVerb_StopUsesSigtermNotBootout(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd-specific spelling")
	}
	ran := stubSupervisor(t)
	stubEndpoint(t, true, false) // up, then goes quiet

	var buf bytes.Buffer
	require.NoError(t, runDaemonVerb(&buf, verbStop))

	require.Len(t, *ran, 1)
	assert.Contains(t, (*ran)[0], "kill SIGTERM")
	assert.NotContains(t, (*ran)[0], "bootout",
		"bootout unloads the job, so `mache daemon start` could not bring it back")
}

// TestSupervisorArgv_HasNoReloadVerb documents an absence on purpose.
//
// launchd has no reload primitive; systemd's works only for units declaring
// ExecReload, which mache's does not; and the daemon has no config to re-read
// without restarting, since each session resolves its own root and builds its
// own graph on connect. A `reload` verb could only alias restart on one
// platform and fail on the other, so it does not exist.
func TestSupervisorArgv_HasNoReloadVerb(t *testing.T) {
	for _, v := range []supervisorVerb{verbStart, verbStop, verbRestart} {
		assert.NotEqual(t, "reload", v.String())
	}
}

// TestSupervisorArgv_UnsupportedPlatformErrors pins that an unknown platform
// is an ERROR. The switch this replaced fell through with no default, so a
// platform mache cannot drive reported success for work it had not done.
//
// Driven through supervisorArgvFor with an explicit GOOS so the branch is
// actually reachable. Keyed on runtime.GOOS this test could only skip on the
// two supported platforms — and a mutation returning bogus success from that
// branch survived, which is exactly what an always-skipped test buys you.
func TestSupervisorArgv_UnsupportedPlatformErrors(t *testing.T) {
	for _, goos := range []string{"windows", "plan9", "js"} {
		_, _, err := supervisorArgvFor(goos, verbStart, false)
		require.ErrorIsf(t, err, errUnsupportedSupervisor, "goos %q", goos)
		assert.Contains(t, err.Error(), "launchd",
			"the error must name what mache DOES support, not just what it does not")
	}
}

// shrinkSettle collapses the settle window so a negative case proves "it never
// came up" in milliseconds instead of spending the real 20s.
func shrinkSettle(t *testing.T) {
	t.Helper()
	prevTimeout, prevPoll := daemonSettleTimeout, daemonSettlePoll
	daemonSettleTimeout = 30 * time.Millisecond
	daemonSettlePoll = 5 * time.Millisecond
	t.Cleanup(func() {
		daemonSettleTimeout, daemonSettlePoll = prevTimeout, prevPoll
	})
}

// TestRunDaemonVerb_RestartIsANoOpWithNoSupervisedDaemon pins a contract that
// predates this file and that adding verification briefly broke.
//
// `task install` runs `mache daemon restart` unconditionally, so on any machine
// that never ran `mache init --global` — every CI runner — turning "no daemon
// to restart" into an error fails the install. Caught by install-verify, where
// systemctl exits 5 for a unit that does not exist.
//
// start is the verb that may start something; restart must not.
func TestRunDaemonVerb_RestartIsANoOpWithNoSupervisedDaemon(t *testing.T) {
	prev := querySupervisorCmd
	querySupervisorCmd = func(string, ...string) (string, error) {
		return "", errors.New("no such unit") // nothing loaded
	}
	t.Cleanup(func() { querySupervisorCmd = prev })

	ran := stubSupervisor(t)
	stubEndpoint(t, false)
	shrinkSettle(t)

	var buf bytes.Buffer
	require.NoError(t, runDaemonVerb(&buf, verbRestart),
		"restart with nothing supervised must SUCCEED; task install runs it unconditionally")
	assert.Empty(t, *ran, "and must not reach the supervisor at all")
	assert.Contains(t, buf.String(), "nothing to restart")
}

// TestRunDaemonVerb_StartStillActsWithNoSupervisedDaemon is the other half:
// the no-op above must not leak into start, whose entire purpose is to bring up
// a daemon that is not running.
func TestRunDaemonVerb_StartStillActsWithNoSupervisedDaemon(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("supervisor verbs are Unix-only")
	}
	prev := querySupervisorCmd
	querySupervisorCmd = func(string, ...string) (string, error) {
		return "", errors.New("no such unit")
	}
	t.Cleanup(func() { querySupervisorCmd = prev })

	ran := stubSupervisor(t)
	stubEndpoint(t, true)

	var buf bytes.Buffer
	require.NoError(t, runDaemonVerb(&buf, verbStart))
	require.Len(t, *ran, 1, "start must still reach the supervisor")
}
