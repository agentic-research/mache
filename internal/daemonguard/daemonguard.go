// Package daemonguard is the supervised daemon's crash-loop circuit breaker.
//
// Why it exists (mache-956488): both supervisors respawn a failing daemon
// forever. launchd's KeepAlive{SuccessfulExit:false} + ThrottleInterval=10
// means a genuinely broken binary relaunches every ten seconds until the user
// notices — and launchd has no StartLimitBurst equivalent to stop it. systemd
// has one, so the unit configures it, but that leaves the platform mache is
// actually developed on with no bound at all.
//
// The portable mechanism is the supervisors' own contract: BOTH treat a clean
// exit as "do not respawn" (KeepAlive{SuccessfulExit:false} on darwin,
// Restart=on-failure on linux). So the daemon breaks its own loop by exiting
// ZERO once it observes too many unclean starts in a window — converting an
// unbounded respawn storm into a bounded one that ends with a loud log line.
//
// Fail-open by construction: an unreadable, corrupt, or unwritable state file
// yields "do not trip". A broken breaker must never be the reason a healthy
// daemon refuses to start.
package daemonguard

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/agentic-research/mache/internal/projcfg"
)

const (
	// stateFile records recent start attempts beside the rest of mache's
	// per-machine state.
	stateFile = "daemon-starts.json"

	// maxEntries bounds the file regardless of window arithmetic, so a clock
	// jump cannot grow it without limit.
	maxEntries = 64
)

// Window and Burst define the loop: this many unclean starts inside this
// window is a crash loop, not a busy afternoon. Sized against the supervisors'
// own retry cadence (ThrottleInterval=10 / RestartSec=10): a real loop trips
// in ~50s, while a human restarting a HEALTHY daemon never does — a healthy
// run marks itself clean on the way out.
//
// Overridable by environment for the same reason the settle and drain
// timeouts are: the hermetic launchd E2E has to watch a real loop actually
// end, and waiting out the production bounds would make that test unrunnable.
var (
	Window = projcfg.EnvDurationOr("MACHE_DAEMON_BREAKER_WINDOW", 2*time.Minute)
	Burst  = projcfg.EnvIntOr("MACHE_DAEMON_BREAKER_BURST", 5)
)

// startRecord is one observed start of the supervised daemon.
type startRecord struct {
	At    int64 `json:"at"`  // unix seconds
	PID   int   `json:"pid"` // the recording process
	Clean bool  `json:"clean"`
}

type state struct {
	Starts []startRecord `json:"starts"`
}

// SupervisedEnv is the environment variable the generated plist/unit sets on
// the daemon. The breaker arms ONLY when it is set: a hand-run `mache serve`
// that keeps failing is the operator's business, and must never be refused.
const SupervisedEnv = "MACHE_SUPERVISED"

// Supervised reports whether this process was started by mache's own
// supervisor definition.
func Supervised() bool { return os.Getenv(SupervisedEnv) == "1" }

func statePath() (string, error) {
	home, err := projcfg.MacheHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, stateFile), nil
}

// load reads the breaker's state, yielding the zero value on EVERY error.
//
// Structurally this twins projcfg.LoadProjectRegistry — same read-JSON-under-
// ~/.mache shape, which the duplicate-definitions gate flags and which is
// baselined deliberately. The two have OPPOSITE error contracts and must not
// be merged: the registry surfaces failures to its caller (a corrupt registry
// is a fact the caller has to handle), while this one fails open by design,
// because a breaker that cannot read its own bookkeeping must never be the
// reason a healthy daemon refuses to start. A shared helper would force one
// of those semantics onto the other.
func load() state {
	var st state
	path, err := statePath()
	if err != nil {
		return st
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	if json.Unmarshal(data, &st) != nil {
		return state{} // corrupt: fail open
	}
	return st
}

func (st state) save() {
	path, err := statePath()
	if err != nil {
		return
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	_ = projcfg.WriteFileAtomic(path, append(data, '\n'))
}

// prune drops records outside the window and caps the list length.
func prune(recs []startRecord, now time.Time) []startRecord {
	cutoff := now.Add(-Window).Unix()
	out := make([]startRecord, 0, len(recs))
	for _, r := range recs {
		if r.At >= cutoff && r.At <= now.Unix()+1 { // +1s slack for clock granularity
			out = append(out, r)
		}
	}
	if len(out) > maxEntries {
		out = out[len(out)-maxEntries:]
	}
	return out
}

// RecordStart notes this start and reports whether the breaker should trip —
// i.e. whether enough PREVIOUS starts inside the window ended without a clean
// exit. The caller exits zero on true, which is how both supervisors are told
// to stop respawning.
//
// The record for this start is written as unclean; MarkCleanExit flips it. A
// process that dies before it can mark itself therefore counts as a crash,
// which is exactly the intent — including a SIGKILL (the codesigning-kill
// family, mache-706d8f) that leaves no chance to run cleanup.
func RecordStart(now time.Time) (trip bool, unclean int) {
	st := load()
	st.Starts = prune(st.Starts, now)
	for _, r := range st.Starts {
		if !r.Clean {
			unclean++
		}
	}
	st.Starts = append(st.Starts, startRecord{At: now.Unix(), PID: os.Getpid()})
	st.save()
	return unclean >= Burst, unclean
}

// MarkCleanExit records that THIS process is shutting down cleanly, so its
// start does not count toward a future trip.
func MarkCleanExit() {
	st := load()
	pid := os.Getpid()
	for i := range st.Starts {
		if st.Starts[i].PID == pid {
			st.Starts[i].Clean = true
		}
	}
	st.save()
}

// Reset clears the record. Called when a HUMAN asks for a start or restart: a
// tripped breaker must never be an unrecoverable state, and an explicit verb
// is the signal that someone has looked at the problem.
func Reset() {
	state{}.save()
}

// Report is what doctor surfaces about the breaker.
type Report struct {
	UncleanStarts int
	Window        time.Duration
	Burst         int
	Tripped       bool
}

// Status describes the current breaker state without recording anything.
// ok=false means there is nothing to report (no state file yet).
func Status(now time.Time) (Report, bool) {
	st := load()
	recs := prune(st.Starts, now)
	if len(recs) == 0 {
		return Report{}, false
	}
	unclean := 0
	for _, r := range recs {
		if !r.Clean {
			unclean++
		}
	}
	return Report{
		UncleanStarts: unclean,
		Window:        Window,
		Burst:         Burst,
		Tripped:       unclean >= Burst,
	}, true
}

// TripMessage is the loud log line a tripping daemon emits before exiting
// zero. It names the state, the mechanism, and the way out — a silent stop
// would be indistinguishable from the daemon simply not being installed.
func TripMessage(unclean int) string {
	return fmt.Sprintf(
		"crash-loop breaker TRIPPED: %d unclean daemon starts within %s (limit %d). "+
			"Exiting 0 so the supervisor stops respawning — this is a bounded stop, not a running daemon. "+
			"The failures are above this line in the daemon log (`mache doctor` names its path); "+
			"fix the cause, then `mache daemon start` clears the breaker and retries.",
		unclean, Window, Burst)
}
