package daemonguard

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentic-research/mache/internal/projcfg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hermeticHome points HOME at a temp dir so the breaker's state file never
// touches the real ~/.mache (projcfg's guard would refuse anyway — this makes
// the intent local and the tests independent).
func hermeticHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

// crashN simulates n starts that never reached a clean exit, spread one
// supervisor-retry apart, ending at now. Returns the trip verdict of a
// FURTHER start — the one that would be the (n+1)th.
func crashN(t *testing.T, n int, now time.Time) (bool, int) {
	t.Helper()
	for i := n; i > 0; i-- {
		// Each crashed run is a distinct process; the breaker keys clean-exit
		// marking on pid, so reusing our own pid would let one MarkCleanExit
		// rewrite history. Write the records directly at distinct pids.
		st := load()
		st.Starts = append(st.Starts, startRecord{At: now.Add(-time.Duration(i) * 10 * time.Second).Unix(), PID: 900000 + i})
		st.save()
	}
	return RecordStart(now)
}

// TestRecordStart_TripsOnlyAfterBurstUncleanStarts pins the core contract: a
// crash loop trips, and it trips at the declared limit — not earlier (which
// would refuse a merely busy machine) and not never.
func TestRecordStart_TripsOnlyAfterBurstUncleanStarts(t *testing.T) {
	now := time.Now()

	t.Run("one short of the limit does not trip", func(t *testing.T) {
		hermeticHome(t)
		trip, unclean := crashN(t, Burst-1, now)
		assert.False(t, trip, "the breaker must not fire before the declared limit")
		assert.Equal(t, Burst-1, unclean)
	})

	t.Run("at the limit it trips", func(t *testing.T) {
		hermeticHome(t)
		trip, unclean := crashN(t, Burst, now)
		assert.True(t, trip, "%d unclean starts in the window IS the crash loop", Burst)
		assert.Equal(t, Burst, unclean)
	})
}

// TestMarkCleanExit_HealthyRunsNeverAccumulate is the false-positive guard:
// a daemon that starts, serves, and shuts down cleanly must never trip the
// breaker however many times it is restarted. Without this the breaker would
// punish `task install` loops.
func TestMarkCleanExit_HealthyRunsNeverAccumulate(t *testing.T) {
	hermeticHome(t)
	now := time.Now()

	for i := 0; i < Burst*3; i++ {
		trip, _ := RecordStart(now.Add(time.Duration(i) * time.Second))
		require.Falsef(t, trip, "healthy restart %d must not trip", i)
		MarkCleanExit() // the graceful-shutdown path
	}

	rep, ok := Status(now.Add(time.Minute))
	require.True(t, ok)
	assert.Zero(t, rep.UncleanStarts, "every run marked itself clean")
	assert.False(t, rep.Tripped)
}

// TestPrune_WindowBoundsTheCount pins that the breaker forgets: crashes from
// last week are not this afternoon's loop.
func TestPrune_WindowBoundsTheCount(t *testing.T) {
	hermeticHome(t)
	now := time.Now()

	// Burst crashes, but long enough ago to be outside the window.
	st := load()
	for i := 0; i < Burst; i++ {
		st.Starts = append(st.Starts, startRecord{At: now.Add(-Window - time.Hour).Unix(), PID: 800000 + i})
	}
	st.save()

	trip, unclean := RecordStart(now)
	assert.False(t, trip, "crashes outside the window must not trip the breaker")
	assert.Zero(t, unclean)
}

// TestReset_MakesATrippedBreakerRecoverable pins the escape hatch: an
// explicit `mache daemon start` clears the record, so a tripped breaker is
// never an unrecoverable state.
func TestReset_MakesATrippedBreakerRecoverable(t *testing.T) {
	hermeticHome(t)
	now := time.Now()

	trip, _ := crashN(t, Burst, now)
	require.True(t, trip, "precondition: the breaker is tripped")

	Reset()

	trip, unclean := RecordStart(now)
	assert.False(t, trip, "an explicit start must clear the breaker and be allowed to try")
	assert.Zero(t, unclean)
}

// TestFailsOpen pins the direction of every failure mode: a breaker that
// cannot read its own state must let the daemon start. Refusing to start
// because the bookkeeping is broken would be strictly worse than the crash
// loop it exists to bound.
func TestFailsOpen(t *testing.T) {
	t.Run("corrupt state file", func(t *testing.T) {
		home := hermeticHome(t)
		require.NoError(t, os.MkdirAll(filepath.Join(home, ".mache"), 0o700))
		path := filepath.Join(home, ".mache", stateFile)
		require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o644))

		trip, unclean := RecordStart(time.Now())
		assert.False(t, trip, "unreadable bookkeeping must never block a start")
		assert.Zero(t, unclean)
	})

	t.Run("no state at all", func(t *testing.T) {
		hermeticHome(t)
		trip, unclean := RecordStart(time.Now())
		assert.False(t, trip)
		assert.Zero(t, unclean)
	})
}

// TestSupervised_ArmsOnlyUnderOurOwnSupervisorDefinition pins the gate: the
// breaker exists to bound an AUTOMATIC respawn loop. A human running
// `mache serve` in a terminal must never be refused, however many times they
// have failed.
func TestSupervised_ArmsOnlyUnderOurOwnSupervisorDefinition(t *testing.T) {
	t.Setenv(SupervisedEnv, "")
	assert.False(t, Supervised(), "an unset marker means a hand-run daemon")

	t.Setenv(SupervisedEnv, "1")
	assert.True(t, Supervised())

	t.Setenv(SupervisedEnv, "0")
	assert.False(t, Supervised(), "only the exact arming value counts")
}

// TestStatus_ReportsWithoutRecording pins that doctor's read is a read: asking
// about the breaker must not itself count as a start.
func TestStatus_ReportsWithoutRecording(t *testing.T) {
	hermeticHome(t)
	now := time.Now()
	_, _ = crashN(t, 2, now)

	before, ok := Status(now)
	require.True(t, ok)
	for i := 0; i < 5; i++ {
		_, _ = Status(now)
	}
	after, ok := Status(now)
	require.True(t, ok)
	assert.Equal(t, before.UncleanStarts, after.UncleanStarts,
		"Status must not mutate the record it reports on")
}

// TestStateLivesUnderMacheHome pins the location through the shared seam, so
// the breaker inherits projcfg's test-hermeticity guard rather than deriving
// its own path (mache-3e78d2).
func TestStateLivesUnderMacheHome(t *testing.T) {
	home := hermeticHome(t)
	_, _ = RecordStart(time.Now())

	got, err := statePath()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".mache", stateFile), got)
	assert.FileExists(t, got)

	// And the seam is projcfg's, not a private copy.
	mh, err := projcfg.MacheHomeDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(mh, stateFile), got)
}

// TestTripMessage_NamesStateMechanismAndWayOut pins the one line an operator
// actually sees when the breaker fires. A silent stop is indistinguishable
// from "the daemon was never installed", so the message must say WHAT
// happened, that the stop is deliberate, and how to get out of it.
func TestTripMessage_NamesStateMechanismAndWayOut(t *testing.T) {
	msg := TripMessage(7)

	assert.Contains(t, msg, "TRIPPED", "the state must be named, not implied")
	assert.Contains(t, msg, "7", "the observed count must appear so the operator can judge it")
	assert.Contains(t, msg, "Exiting 0",
		"the mechanism must be stated — exiting zero looks like success in a log otherwise")
	assert.Contains(t, msg, "not a running daemon",
		"a bounded stop must not be mistaken for a healthy daemon")
	assert.Contains(t, msg, "mache daemon start",
		"the way out must be named, or the operator reads the stop as terminal")
}
