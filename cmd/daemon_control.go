package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/agentic-research/mache/internal/daemonguard"
	"github.com/agentic-research/mache/internal/projcfg"
)

// daemonSettleTimeout bounds how long a start/restart waits for the daemon to
// answer, and how long a stop waits for it to go quiet.
//
// Generous relative to doctorProbeTimeout because this is not a diagnostic: a
// supervisor may legitimately delay a launch. launchd's ThrottleInterval on
// mache's own plist is 10s, so anything shorter would report failure for a
// restart that was merely throttled.
//
// 90s because the FIRST exec of newly-written bytes is far slower than a
// restart of the same binary, and `task install` produces exactly that.
// Measured on an Apple Silicon Mac:
//
//	unchanged binary       0s
//	freshly built bytes   10s
//	after a full install  43s   (exceeded the old 20s budget and reported failure
//	                             for a daemon that came back fine)
//
// macOS assesses a binary it has not seen before (Gatekeeper/syspolicyd), and
// that cost lands on the first launch after any install. An earlier attempt to
// rule this out re-signed IDENTICAL content and measured 1s — a cache hit, not
// a refutation. The budget has to cover the slow path or it reports failure for
// success, which is the defect this whole surface exists to remove.
//
// The wait announces itself after daemonSettleAnnounceAfter, so a long budget
// is not a silent one.
var daemonSettleTimeout = projcfg.EnvDurationOr("MACHE_DAEMON_SETTLE", 90*time.Second)

// daemonSettlePoll is the gap between endpoint probes while settling.
//
// Both are vars so tests can shrink them: a negative case must not spend the
// real settle window proving that nothing came up.
var daemonSettlePoll = 250 * time.Millisecond

// errUnsupportedSupervisor is returned on a platform with no supervisor mache
// knows how to drive. Deliberately an ERROR rather than a silent no-op: the
// switch in this file previously fell through with no default, so an unknown
// platform reported success for work it had not done.
var errUnsupportedSupervisor = errors.New(
	"no supported service supervisor: mache drives launchd (macOS) and systemd --user (Linux)")

// supervisorVerb is a platform-independent daemon operation.
type supervisorVerb int

const (
	verbStart supervisorVerb = iota
	verbStop
	verbRestart
)

// supervisorVerbNames is indexed by supervisorVerb. These strings are not
// cosmetic: they are the literal systemd subcommands (`systemctl --user
// <verb> mache.service`), so renaming one changes behaviour on Linux.
var supervisorVerbNames = [...]string{
	verbStart:   "start",
	verbStop:    "stop",
	verbRestart: "restart",
}

func (v supervisorVerb) String() string { return supervisorVerbNames[v] }

// supervisorArgv resolves verb to the concrete command for this platform.
//
// The launchd and systemd spellings are NOT interchangeable, and the
// differences are the point of having this in one place:
//
//   - start: launchd needs `bootstrap` when the job is unloaded and
//     `kickstart` when it is loaded-but-idle; systemd's `start` covers both.
//   - stop: `kill SIGTERM` rather than `bootout`, because bootout UNLOADS the
//     job and a later `kickstart` would then fail. SIGTERM alone is enough to
//     make a stop stick here: mache's plist declares
//     KeepAlive{SuccessfulExit:false}, so launchd resurrects the job only on a
//     NON-zero exit, and cmd/serve.go handles SIGTERM and exits cleanly.
//   - restart: `kickstart -k` kills and relaunches in one step.
//
// There is deliberately no `reload`. launchd has no reload primitive, systemd's
// works only for units declaring ExecReload (mache's does not), and the daemon
// has no config to re-read without restarting — each session resolves its own
// root and builds its own graph on connect. A `reload` verb could only alias
// restart on one platform and fail on the other.
func supervisorArgv(verb supervisorVerb, loaded bool) ([]supervisorStep, error) {
	return supervisorArgvFor(runtime.GOOS, verb, loaded)
}

// supervisorArgvFor takes the platform explicitly so the unsupported-platform
// branch is REACHABLE in a test. Keyed on runtime.GOOS it was not: a test for
// it could only skip on darwin and linux, and an always-skipped test is
// indistinguishable from no test — verified by mutation, where returning a
// bogus success from that branch survived.
// supervisorArgvFor returns the COMMAND SEQUENCE for verb — several steps on
// darwin, one on linux.
//
// darwin start/restart are a RELOAD (bootout → bootstrap → kickstart), never a
// bare `kickstart -k`, because launchd pins the job's code identity at
// bootstrap time. mache's binaries are ad-hoc signed — no Team ID, so identity
// is effectively the CDHash, which changes on EVERY build — and kickstarting a
// job whose binary was replaced relaunches it under the OLD pinned identity.
// The kernel then SIGKILLs the new binary at exec: observed as a crash report
// with `SIGKILL (Code Signature Invalid)`, termination namespace CODESIGNING
// code 4 "Launch Constraint Violation", and launchd throttle-looping the
// respawn — which is what the unexplained 10s/43s/112s restart gaps were
// (mache-706d8f). Reloading re-reads the binary and re-pins.
//
// The trailing kickstart is required: RunAtLoad did NOT fire on bootstrap when
// verified live — the job loaded as "not running, never exited" until kicked.
//
// bootout of an unloaded job fails; that step tolerates failure (okFail) since
// "was not loaded" is exactly the state bootstrap wants.
func supervisorArgvFor(goos string, verb supervisorVerb, loaded bool) ([]supervisorStep, error) {
	target := fmt.Sprintf("gui/%d/%s", os.Getuid(), launchAgentLabel)
	switch goos {
	case "darwin":
		launchctl, err := exec.LookPath("launchctl")
		if err != nil {
			return nil, fmt.Errorf("launchctl not found: %w", err)
		}
		domain := fmt.Sprintf("gui/%d", os.Getuid())
		plist := launchAgentPlistPath()
		if plist == "" {
			return nil, fmt.Errorf("cannot resolve the LaunchAgent plist path (no home directory)")
		}
		switch verb {
		case verbStop:
			return []supervisorStep{{name: launchctl, args: []string{"kill", "SIGTERM", target}}}, nil
		default: // start and restart: reload
			return []supervisorStep{
				{name: launchctl, args: []string{"bootout", target}, okFail: true, awaitGone: true},
				{name: launchctl, args: []string{"bootstrap", domain, plist}},
				{name: launchctl, args: []string{"kickstart", target}},
			}, nil
		}
	case "linux":
		systemctl, err := exec.LookPath("systemctl")
		if err != nil {
			return nil, fmt.Errorf("systemctl not found: %w", err)
		}
		return []supervisorStep{{name: systemctl, args: []string{"--user", verb.String(), "mache.service"}}}, nil
	}
	return nil, errUnsupportedSupervisor
}

// supervisorStep is one supervisor invocation in a verb's sequence.
type supervisorStep struct {
	name string
	args []string
	// okFail marks a step whose failure is an acceptable state, not an error —
	// bootout of a job that is not loaded.
	okFail bool
	// awaitGone marks a step (bootout) whose EFFECT is asynchronous: launchctl
	// returns once removal is INITIATED, while the outgoing process drains for
	// up to ExitTimeOut. Bootstrapping while the label still exists fails with
	// EIO — caught by the real-launchd E2E on its first run, after every
	// stub-level test passed. The runner polls the job away before moving on.
	awaitGone bool
}

// daemonDrainTimeout bounds how long a reload waits, after bootout, for the
// outgoing job to actually leave the launchd domain. The plist's ExitTimeOut
// is 45s (matching TimeoutStopSec on the systemd side), so a draining daemon
// can legally take that long; beyond it launchd SIGKILLs, so gone-ness is
// guaranteed shortly after — 50s covers the whole legal window.
var (
	daemonDrainTimeout = projcfg.EnvDurationOr("MACHE_DAEMON_DRAIN", 50*time.Second)
	daemonDrainPoll    = 100 * time.Millisecond
)

// awaitJobGone polls until the supervised job has left the domain. Read-side
// verification, same philosophy as awaitDaemon: the state we need is "label
// gone", so that is what we observe — not the exit status of the command that
// merely requested it.
func awaitJobGone() bool {
	deadline := time.Now().Add(daemonDrainTimeout)
	for {
		if !querySupervisorJob().Loaded {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(daemonDrainPoll)
	}
}

// daemonEndpointUp reports whether the MCP endpoint answers right now.
//
// A var for the same reason runSupervisorCmd is: tests must be able to drive
// the verify logic without a live daemon. Without the seam every test that
// reaches a restart path would block for daemonSettleTimeout on any machine
// where the daemon is not running — and would PASS quickly only on a developer
// machine that happened to have one up, which is the worst of both.
var daemonEndpointUp = func() (string, bool) {
	v, err := probeDaemonVersion(context.Background())
	return v, err == nil
}

// awaitDaemon polls the endpoint until it reaches want (up or down), returning
// the last observed version and whether the wait succeeded.
//
// Every verb goes through this. Reporting success on the SUPERVISOR command's
// exit status is what made `mache daemon restart` claim it had restarted a
// daemon that was dead: `launchctl kickstart` exiting 0 means the request was
// accepted, not that anything is listening (mache-609a10). Observed after a
// real `task install` — success printed, launchctl showed no PID and
// LastExitStatus 9, and it stayed down.
func awaitDaemon(w io.Writer, up bool) (string, bool) {
	start := time.Now()
	deadline := start.Add(daemonSettleTimeout)
	var version string
	announced := false
	for {
		v, ok := daemonEndpointUp()
		if ok {
			version = v
		}
		if ok == up {
			return version, true
		}
		if time.Now().After(deadline) {
			return version, false
		}
		// Say something once it stops being instant. A bounded wait that
		// prints nothing for 20s is reported as a hang — which is exactly how
		// this surfaced ("mache daemon start appears to hang"). Silence and
		// wedged look identical from the outside, so the wait names itself.
		if !announced && time.Since(start) > daemonSettleAnnounceAfter {
			announced = true
			what := "to answer"
			if !up {
				what = "to stop"
			}
			logf(w, "waiting up to %s for the daemon %s at %s…\n",
				daemonSettleTimeout, what, projcfg.MacheHTTPURL)
		}
		time.Sleep(daemonSettlePoll)
	}
}

// daemonSettleAnnounceAfter is how long a settle may stay silent before it
// tells the caller what it is doing.
var daemonSettleAnnounceAfter = 1500 * time.Millisecond

// runDaemonVerb executes verb and then VERIFIES the state it claims to have
// produced, rather than trusting the supervisor's exit status.
func runDaemonVerb(w io.Writer, verb supervisorVerb) error {
	job := querySupervisorJob()

	// restart is restart-IF-RUNNING and stays a successful no-op when there is
	// no supervised daemon. That contract predates this file and is load
	// bearing: `task install` runs `mache daemon restart` unconditionally, so
	// on a machine that never ran `mache init --global` — every CI runner —
	// making this an error fails the install. Regression caught by
	// install-verify, where systemctl exits 5 for a unit that does not exist.
	//
	// Starting a daemon nobody asked for is `mache daemon start`'s job, which
	// is exactly why that verb now exists separately.
	if verb == verbRestart && !job.Running {
		logf(w, "no supervised daemon is running; nothing to restart\n")
		return nil
	}

	if verb == verbStop && !job.Running {
		if _, up := daemonEndpointUp(); !up {
			logf(w, "daemon is already stopped\n")
			return nil
		}
	}

	// A human asking for start/restart is the signal that someone has looked
	// at the problem, so it clears the crash-loop breaker (mache-956488). A
	// tripped breaker must never be an unrecoverable state: without this, a
	// daemon that tripped would refuse every subsequent start and report the
	// refusal as a failed verb.
	if verb != verbStop {
		daemonguard.Reset()
	}

	steps, err := supervisorArgv(verb, job.Loaded)
	if err != nil {
		return err
	}
	for _, st := range steps {
		runErr := runSupervisorCmd(st.name, st.args...)
		if runErr == nil || st.okFail {
			// The step's effect may lag its exit status: bootout returns once
			// removal is initiated. Verify the state the NEXT step depends on.
			if st.awaitGone && !awaitJobGone() {
				return fmt.Errorf(
					"asked the supervisor to remove the old daemon job and it accepted, "+
						"but the job is still present after %s — the outgoing daemon did not drain; "+
						"check the log: %s", daemonDrainTimeout, daemonLogHint())
			}
			continue
		}
		// launchctl kill returns ESRCH when nothing is running; that is
		// the state stop wanted, not a failure.
		if verb == verbStop && errors.Is(runErr, syscall.ESRCH) {
			logf(w, "daemon was not running\n")
			return nil
		}
		return fmt.Errorf("%s the supervised daemon (%s %s): %w",
			verb, filepath.Base(st.name), strings.Join(st.args, " "), runErr)
	}

	wantUp := verb != verbStop
	version, ok := awaitDaemon(w, wantUp)
	if !ok {
		if wantUp {
			return fmt.Errorf(
				"asked the supervisor to %s the daemon and it accepted, but nothing is answering at %s after %s.\n"+
					"Check the log for why it exited: %s",
				verb, projcfg.MacheHTTPURL, daemonSettleTimeout, daemonLogHint())
		}
		return fmt.Errorf("asked the supervisor to stop the daemon and it accepted, but %s is still answering after %s",
			projcfg.MacheHTTPURL, daemonSettleTimeout)
	}

	if wantUp {
		logf(w, "daemon %sed: answering at %s, serving %s\n", verb, projcfg.MacheHTTPURL, version)
		return nil
	}
	logf(w, "daemon stopped: nothing answering at %s\n", projcfg.MacheHTTPURL)
	return nil
}

// reportDaemonStatus prints what the supervisor thinks and what the endpoint
// actually does. Both, because they disagree in exactly the case worth
// catching: a job the supervisor considers loaded whose process is gone.
func reportDaemonStatus(w io.Writer) error {
	job := querySupervisorJob()
	switch {
	case !job.Loaded:
		logf(w, "supervisor: not installed (run `mache init --global` to install it)\n")
	case job.Running:
		logf(w, "supervisor: running, program %s\n", job.Program)
	default:
		logf(w, "supervisor: installed but not running, program %s\n", job.Program)
	}

	version, up := daemonEndpointUp()
	if up {
		logf(w, "endpoint:   answering at %s, serving %s\n", projcfg.MacheHTTPURL, version)
		return nil
	}
	logf(w, "endpoint:   not answering at %s\n", projcfg.MacheHTTPURL)
	if job.Loaded && !job.Running {
		logf(w, "            → mache daemon start\n")
	}
	return nil
}
