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
	// Platform-conditional on purpose: darwin start is a three-step reload
	// (bootout, bootstrap, kickstart — launchd pins code identity at
	// bootstrap), linux is systemd's single `start`. The first version of this
	// line asserted 3 unconditionally and failed on ubuntu CI — a
	// platform-blind assertion whose own message said "darwin".
	if runtime.GOOS == "darwin" {
		require.Len(t, *ran, 3, "darwin start is a reload: bootout, bootstrap, kickstart")
	} else {
		require.Len(t, *ran, 1, "linux start is a single systemctl invocation")
	}
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
		"stop must leave the job LOADED — bootout without a following bootstrap "+
			"would strand `mache daemon start`")
}

// TestRunDaemonVerb_StartAndRestartReloadTheJob pins the fix for the
// CODESIGNING kill family (mache-706d8f).
//
// launchd pins the job's code identity at bootstrap. mache is ad-hoc signed, so
// identity is effectively the CDHash — different for EVERY build — and a bare
// `kickstart -k` after the binary is replaced relaunches under the OLD pinned
// identity: the kernel SIGKILLs the new binary at exec ("Launch Constraint
// Violation", from a live crash report), and launchd's respawn throttling turns
// that into the unexplained 10s/43s/112s restart gaps. The sequence must
// therefore be bootout -> bootstrap -> kickstart, and the trailing kickstart is
// load-bearing: RunAtLoad does not fire on bootstrap (verified live).
func TestRunDaemonVerb_StartAndRestartReloadTheJob(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd identity pinning is darwin-specific")
	}
	for _, verb := range []supervisorVerb{verbStart, verbRestart} {
		t.Run(verb.String(), func(t *testing.T) {
			ran := stubSupervisor(t)

			// restart requires a running job or it no-ops — and once the
			// reload's bootout has run, the job is GONE and the state query
			// must say so, or awaitJobGone waits out the full drain window
			// polling a stub frozen in the pre-verb world.
			prev := querySupervisorCmd
			querySupervisorCmd = func(string, ...string) (string, error) {
				for _, step := range *ran {
					if strings.Contains(step, "bootout") {
						return "", errors.New("no such service")
					}
				}
				return "mache = {\n\tstate = running\n\tprogram = /x/mache\n}\n", nil
			}
			t.Cleanup(func() { querySupervisorCmd = prev })

			stubEndpoint(t, true)
			shrinkSettle(t)

			var buf bytes.Buffer
			require.NoError(t, runDaemonVerb(&buf, verb))

			require.Len(t, *ran, 3, "reload is three steps")
			assert.Contains(t, (*ran)[0], "bootout")
			assert.Contains(t, (*ran)[1], "bootstrap")
			assert.Contains(t, (*ran)[2], "kickstart")
			assert.NotContains(t, (*ran)[2], "-k",
				"the job was just re-read; -k would be a no-op kill of nothing")
		})
	}
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
		_, err := supervisorArgvFor(goos, verbStart, false)
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
	prevDrain, prevDrainPoll := daemonDrainTimeout, daemonDrainPoll
	daemonSettleTimeout = 30 * time.Millisecond
	daemonSettlePoll = 5 * time.Millisecond
	// The drain wait polls querySupervisorJob after bootout. Tests whose
	// scripted supervisor never changes state would otherwise sit out the
	// full drain window on every restart path; the conformance table models
	// the state change properly, and everything else just needs speed.
	daemonDrainTimeout = 30 * time.Millisecond
	daemonDrainPoll = 5 * time.Millisecond
	t.Cleanup(func() {
		daemonSettleTimeout, daemonSettlePoll = prevTimeout, prevPoll
		daemonDrainTimeout, daemonDrainPoll = prevDrain, prevDrainPoll
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
	require.NotEmpty(t, *ran, "start must still reach the supervisor")
}

// TestRunDaemonVerb_AnnouncesALongWait covers a failure that is not a crash:
// a bounded wait that prints nothing.
//
// start/restart may legitimately take seconds while the supervisor relaunches,
// and the old code sat silent for the whole settle window. Reported from a
// real machine as "mache daemon start appears to hang" — from the outside,
// silence and wedged are the same observation, so the wait has to name itself.
func TestRunDaemonVerb_AnnouncesALongWait(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("supervisor verbs are Unix-only")
	}
	stubSupervisor(t)
	// Never comes up, so the settle runs to its deadline.
	stubEndpoint(t, false)

	prevAnnounce := daemonSettleAnnounceAfter
	prevTimeout, prevPoll := daemonSettleTimeout, daemonSettlePoll
	daemonSettleAnnounceAfter = 5 * time.Millisecond
	daemonSettleTimeout = 60 * time.Millisecond
	daemonSettlePoll = 5 * time.Millisecond
	t.Cleanup(func() {
		daemonSettleAnnounceAfter = prevAnnounce
		daemonSettleTimeout, daemonSettlePoll = prevTimeout, prevPoll
	})

	var buf bytes.Buffer
	require.Error(t, runDaemonVerb(&buf, verbStart))

	assert.Contains(t, buf.String(), "waiting up to",
		"a wait longer than a blink must say it is waiting")
	assert.Contains(t, buf.String(), macheHTTPURL,
		"and say what it is waiting ON, so the reader can check it themselves")
}

// TestRunDaemonVerb_StaysQuietWhenItSettlesImmediately is the other half: the
// announcement must not fire on the common fast path, or it becomes noise that
// trains people to ignore it.
func TestRunDaemonVerb_StaysQuietWhenItSettlesImmediately(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("supervisor verbs are Unix-only")
	}
	stubSupervisor(t)
	// Down, then up: one poll of real waiting, far under the announce
	// threshold. Settling on the FIRST probe would return before the guard is
	// ever reached, so the test would pass even for code that always
	// announces — verified by mutation.
	stubEndpoint(t, false, true)

	var buf bytes.Buffer
	require.NoError(t, runDaemonVerb(&buf, verbStart))
	assert.NotContains(t, buf.String(), "waiting up to",
		"a wait far shorter than the threshold must not announce one")
}

// TestRunBounded_TimesOutInsteadOfWaitingForever pins the bound itself.
//
// The supervisor seam is stubbed in every other test here, so reverting
// exec.CommandContext to exec.Command survives them all — the real
// implementation is never reached. This drives it directly with a command that
// does not return. launchctl blocks exactly like this against a wedged job,
// which is how "mache daemon start appears to hang" was reported.
func TestRunBounded_TimesOutInsteadOfWaitingForever(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("needs a POSIX sleep")
	}
	prev := supervisorCmdTimeout
	supervisorCmdTimeout = 50 * time.Millisecond
	t.Cleanup(func() { supervisorCmdTimeout = prev })

	start := time.Now()
	err := runBounded("/bin/sleep", "30")
	elapsed := time.Since(start)

	require.Error(t, err, "a command that never returns must produce an error, not a hang")
	assert.Less(t, elapsed, 5*time.Second, "must not wait for the command to finish")
	assert.Contains(t, err.Error(), "did not return within",
		"the error must say it timed out, not surface an opaque exec failure")
	assert.Contains(t, err.Error(), "launchctl print",
		"and must hand the operator a way to inspect the wedged supervisor")
}
