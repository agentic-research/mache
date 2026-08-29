package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Lifecycle conformance: the transition table (mache-96465d, layer 1).
//
// Every lifecycle bug this arc shipped lived in an UNTESTED TRANSITION, not an
// untested function: running→install (cp corruption), not-loaded→restart (the
// no-op contract regression that failed CI), running→replace-binary→restart
// (the CODESIGNING kill), every-verb→verify-own-claim (restart reporting
// success over a dead daemon). Verb tests existed; the STATE×VERB grid did not,
// so each new verb re-litigated the contract cell by cell and missed some.
//
// This table IS the contract. A cell asserts three things:
//   1. the verb's error/no-error outcome,
//   2. the supervisor command sequence it may issue (including "none"),
//   3. the LIE DETECTOR: every state-claiming line in the output corresponds
//      to the observed post-state — no output may assert a state the endpoint
//      probe does not confirm. That invariant was retrofitted verb by verb
//      this week; here every current and future cell inherits it.
//
// Deliberately stub-driven and deterministic: the supervisor seam and the
// endpoint probe are scripted, so the table runs identically everywhere. What
// stubs structurally CANNOT represent — launchd's identity pinning, RunAtLoad
// semantics, real SIGTERM delivery — is layer 3's hermetic E2E, not this
// file's job, and pretending otherwise is how the codesigning kill stayed
// invisible under a green suite.

// lifecycleState is the daemon state a cell starts from, expressed as stub
// behaviour.
type lifecycleState struct {
	name string
	// supervisorReply is what querySupervisorCmd returns (launchctl print /
	// systemctl show shapes are parsed upstream; an error means "not loaded").
	supervisorReply func() (string, error)
	// loaded/running are what the reply must PARSE to on the platform running
	// the test — asserted by scriptCell before any verb runs, so a fixture
	// whose shape the local parser cannot read fails naming the fixture.
	loaded, running bool
	// endpointUp scripts daemonEndpointUp answers, held on the last value.
	endpointUp []bool
}

// supervisorReplyFor scripts a reply in the shape the CURRENT platform's
// parser expects. The first version returned launchctl-print shape
// unconditionally; linux's parseSystemctlShow read that as not-loaded, so on
// ubuntu every "running" cell silently became a not-loaded cell and the no-op
// guards fired — the same platform-blind-test class that broke #641's first
// CI run, now caught by the parse self-check in scriptCell instead of by a
// confusing downstream cell failure.
func supervisorReplyFor(running bool) func() (string, error) {
	return func() (string, error) {
		if runtime.GOOS == "linux" {
			active := "inactive"
			if running {
				active = "active"
			}
			return "LoadState=loaded\nActiveState=" + active +
				"\nExecStart={ path=/x/mache ; argv[]=/x/mache }\n", nil
		}
		st := "waiting"
		if running {
			st = "running"
		}
		return "mache = {\n\tstate = " + st + "\n\tprogram = /x/mache\n}\n", nil
	}
}

var (
	stateNotLoaded = lifecycleState{
		name:            "not-loaded",
		supervisorReply: func() (string, error) { return "", errors.New("no such service") },
		// Down, then up: a start from not-loaded bootstraps a daemon that DOES
		// come up. The first version scripted {false} — "never answers" — and
		// the table correctly failed its own author: the cell model must
		// describe the post-verb world, not just the pre-verb one. No-op cells
		// never poll, so the trailing true is inert for them.
		endpointUp: []bool{false, true},
	}
	stateLoadedIdle = lifecycleState{
		name:            "loaded-idle",
		supervisorReply: supervisorReplyFor(false),
		loaded:          true,
		endpointUp:      []bool{false, true}, // comes up once started
	}
	stateRunning = lifecycleState{
		name:            "running",
		supervisorReply: supervisorReplyFor(true),
		loaded:          true,
		running:         true,
		endpointUp:      []bool{true},
	}
)

// conformanceCell is one (state, verb) transition and its postconditions.
type conformanceCell struct {
	state lifecycleState
	verb  supervisorVerb
	// wantErr: the verb must fail. Everything else must succeed — including
	// the no-op cells, whose regression (restart on not-loaded returning an
	// error) broke `task install` on every CI runner.
	wantErr bool
	// wantSupervisorTouched: whether ANY supervisor command may run. The
	// no-op cells must not reach the supervisor at all.
	wantSupervisorTouched bool
	// wantClaims: substrings that MUST appear, each of which is a state claim
	// the lie detector cross-checks against the scripted post-state.
	wantClaims []string
	// forbidClaims: state claims that must NOT appear in this cell.
	forbidClaims []string
}

func lifecycleCells() []conformanceCell {
	return []conformanceCell{
		// --- restart: restart-if-running, verify-if-restarted ---
		{
			state: stateNotLoaded, verb: verbRestart,
			wantSupervisorTouched: false,
			wantClaims:            []string{"nothing to restart"},
			forbidClaims:          []string{"restarted"},
		},
		{
			state: stateLoadedIdle, verb: verbRestart,
			wantSupervisorTouched: false,
			wantClaims:            []string{"nothing to restart"},
			forbidClaims:          []string{"restarted"},
		},
		{
			state: stateRunning, verb: verbRestart,
			wantSupervisorTouched: true,
			wantClaims:            []string{"restarted"},
		},

		// --- start: may start from anywhere; must verify ---
		{
			state: stateNotLoaded, verb: verbStart,
			wantSupervisorTouched: true,
			wantClaims:            []string{"started"},
		},
		{
			state: stateLoadedIdle, verb: verbStart,
			wantSupervisorTouched: true,
			wantClaims:            []string{"started"},
		},
		{
			state: stateRunning, verb: verbStart,
			wantSupervisorTouched: true,
			wantClaims:            []string{"started"},
		},

		// --- stop: must confirm quiet; already-stopped is success ---
		{
			state: stateNotLoaded, verb: verbStop,
			wantSupervisorTouched: false,
			wantClaims:            []string{"already stopped"},
			forbidClaims:          []string{"daemon stopped:"},
		},
		{
			state: stateLoadedIdle, verb: verbStop,
			wantSupervisorTouched: false,
			wantClaims:            []string{"already stopped"},
			forbidClaims:          []string{"daemon stopped:"},
		},
		{
			state: stateRunning, verb: verbStop,
			wantSupervisorTouched: true,
			wantClaims:            []string{"stopped"},
		},
	}
}

// scriptCell wires a cell's state into the seams and returns the recorder.
func scriptCell(t *testing.T, st lifecycleState, verb supervisorVerb) *[]string {
	t.Helper()
	ran := stubSupervisor(t)

	prevQ := querySupervisorCmd
	querySupervisorCmd = func(string, ...string) (string, error) {
		// The stub models the POST-VERB world, not a frozen snapshot: once
		// the verb has issued a bootout, the job is GONE and state queries
		// must say so — the reload's awaitJobGone polls exactly this, and a
		// frozen "still loaded" reply would hang every restart cell for the
		// full drain window (which is precisely how the drain fix announced
		// itself to this table).
		for _, step := range *ran {
			if strings.Contains(step, "bootout") {
				return "", errors.New("no such service")
			}
		}
		return st.supervisorReply()
	}
	t.Cleanup(func() { querySupervisorCmd = prevQ })

	// Fixture lie-detector: the scripted reply must parse, on THIS platform,
	// to the state the cell's name claims. Without it, a shape/parser
	// mismatch surfaces as a wrong-cell-behaviour failure two layers away.
	job := querySupervisorJob()
	require.Equalf(t, st.loaded, job.Loaded,
		"fixture %q must parse as loaded=%v on %s", st.name, st.loaded, runtime.GOOS)
	require.Equalf(t, st.running, job.Running,
		"fixture %q must parse as running=%v on %s", st.name, st.running, runtime.GOOS)

	// The endpoint script: stop-verbs want the endpoint to END down, so the
	// scripted sequence inverts for them once the supervisor acts.
	seq := st.endpointUp
	if verb == verbStop {
		seq = []bool{st.endpointUp[0], false}
	}
	stubEndpoint(t, seq...)
	shrinkSettle(t)
	return ran
}

// TestLifecycleConformance_TransitionTable runs every cell. Adding a verb or a
// state without adding its cells is a compile-time nudge and a review-time
// question; a cell that cannot state its postconditions is a design smell in
// the verb, not the table.
func TestLifecycleConformance_TransitionTable(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("supervisor verbs are Unix-only")
	}
	for _, cell := range lifecycleCells() {
		t.Run(fmt.Sprintf("%s/%s", cell.state.name, cell.verb), func(t *testing.T) {
			ran := scriptCell(t, cell.state, cell.verb)

			var buf bytes.Buffer
			err := runDaemonVerb(&buf, cell.verb)
			out := buf.String()

			if cell.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err, "output:\n%s", out)
			}

			if cell.wantSupervisorTouched {
				assert.NotEmpty(t, *ran, "this transition must act on the supervisor")
			} else {
				assert.Empty(t, *ran,
					"a no-op cell must not reach the supervisor: acting on a state that "+
						"does not need it is how restart-on-not-loaded broke every CI install")
			}

			for _, claim := range cell.wantClaims {
				assert.Contains(t, out, claim)
			}
			for _, claim := range cell.forbidClaims {
				assert.NotContains(t, out, claim,
					"claiming %q from state %s would assert a transition that did not happen",
					claim, cell.state.name)
			}

			assertNoUnverifiedClaims(t, out, err)
		})
	}
}

// assertNoUnverifiedClaims is the generalized lie detector.
//
// Rule: a success-shaped state claim ("daemon started", "daemon restarted",
// "daemon stopped") may only appear in a run that returned nil — an error
// return with a success claim is the exact defect that shipped as "restarted
// the supervised daemon" over a dead endpoint (mache-609a10), retrofitted verb
// by verb this week. Encoding it once here means a future verb cannot ship the
// lie without deleting this test.
func assertNoUnverifiedClaims(t *testing.T, out string, err error) {
	t.Helper()
	successClaims := []string{"daemon started", "daemon restarted", "daemon stopped"}
	for _, c := range successClaims {
		if strings.Contains(out, c) {
			assert.NoError(t, err,
				"output claims %q but the verb returned an error — a claim the "+
					"postcondition does not observe is a lie, not a message", c)
		}
	}
}

// TestLifecycleConformance_FailedVerifyNeverClaimsSuccess drives the lie
// detector from the failure side: the supervisor accepts everything, the
// endpoint never answers, and NO success claim may survive.
func TestLifecycleConformance_FailedVerifyNeverClaimsSuccess(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("supervisor verbs are Unix-only")
	}
	for _, verb := range []supervisorVerb{verbStart, verbRestart} {
		t.Run(verb.String(), func(t *testing.T) {
			scriptCell(t, lifecycleState{
				name:            "running-but-endpoint-dead",
				supervisorReply: stateRunning.supervisorReply,
				loaded:          true,
				running:         true,
				endpointUp:      []bool{false}, // never comes up
			}, verb)

			var buf bytes.Buffer
			err := runDaemonVerb(&buf, verb)

			require.Error(t, err, "an unverified %s must fail", verb)
			assertNoUnverifiedClaims(t, buf.String(), err)
			assert.NotContains(t, buf.String(), verb.String()+"ed",
				"no success claim may survive a failed verification")
		})
	}
}

// TestLifecycleRestart_StuckDrainFailsLoudly pins the drain guard the launchd
// E2E discovered on its first run: `launchctl bootout` returns once removal is
// INITIATED, and bootstrapping while the outgoing job still holds the label
// fails with EIO. The reload must therefore observe the label actually leave
// the domain before bootstrapping — and when the outgoing daemon never
// drains, the verb must say so instead of racing bootstrap into a failure
// that names the wrong step.
func TestLifecycleRestart_StuckDrainFailsLoudly(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the bootout/bootstrap drain race is a launchd sequence")
	}
	// stubSupervisor first — it defaults the read seam to not-loaded, and this
	// cell needs the opposite. Order matters: the override must come after.
	ran := stubSupervisor(t)
	// A supervisor whose job NEVER leaves the domain: state queries report
	// running forever, even after bootout ran.
	prevQ := querySupervisorCmd
	querySupervisorCmd = func(string, ...string) (string, error) {
		return stateRunning.supervisorReply()
	}
	t.Cleanup(func() { querySupervisorCmd = prevQ })
	stubEndpoint(t, true)
	shrinkSettle(t)

	var buf bytes.Buffer
	err := runDaemonVerb(&buf, verbRestart)

	require.Error(t, err, "a reload that cannot clear the old job must fail")
	assert.Contains(t, err.Error(), "did not drain")
	for _, step := range *ran {
		assert.NotContains(t, step, "bootstrap",
			"bootstrap must not run while the old job still holds the label")
	}
	assertNoUnverifiedClaims(t, buf.String(), err)
}
