package cmd

import (
	"testing"
	"time"

	"github.com/agentic-research/mache/internal/daemonguard"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCheckCrashLoop_DistinguishesQuietFromLoopingFromTripped pins the reason
// this check exists (mache-956488): "no daemon answering" and "the daemon gave
// up after five crashes" demand completely different actions, and before this
// an operator saw the same warning for both. Each state must be separately
// legible, and the tripped one must FAIL rather than warn — nothing is
// serving, and that is not a yellow condition.
func TestCheckCrashLoop_DistinguishesQuietFromLoopingFromTripped(t *testing.T) {
	now := time.Now()

	t.Run("quiet", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		got := checkCrashLoop(now)
		assert.Equal(t, statusOK, got.Status)
		assert.Contains(t, got.Detail, "no unclean daemon starts")
	})

	t.Run("restarting but under the limit warns", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		for i := 0; i < daemonguard.Burst-1; i++ {
			_, _ = daemonguard.RecordStart(now.Add(time.Duration(i) * time.Second))
		}
		got := checkCrashLoop(now.Add(time.Minute))
		assert.Equal(t, statusWarn, got.Status,
			"restarting more than it should is a warning, not a failure — something IS serving")
		// Assert the RELATIONSHIP, not a literal: daemonLogHint is a file path
		// on darwin and a journalctl invocation on linux, so hardcoding either
		// makes this test pass on the author's machine and fail on the other
		// platform — which is exactly what it did.
		assert.Contains(t, got.Fix, daemonLogHint(),
			"the fix must name this platform's actual log locator, not a generic phrase to go hunt for")
	})

	t.Run("tripped fails and names the way out", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		for i := 0; i <= daemonguard.Burst; i++ {
			_, _ = daemonguard.RecordStart(now.Add(time.Duration(i) * time.Second))
		}
		got := checkCrashLoop(now.Add(time.Minute))
		require.Equal(t, statusFail, got.Status,
			"a tripped breaker means NOTHING is serving — doctor must not report that as a warning")
		assert.Contains(t, got.Detail, "TRIPPED")
		assert.Contains(t, got.Fix, "mache daemon start",
			"the fix must name the command that clears the breaker, or the state reads as terminal")
	})
}

// TestCheckVersionSkew_DoesNotRecommendTheCommandThatFails pins a remediation
// bug found by running `mache doctor` rather than by reading it.
//
// The check fired correctly — the daemon was serving older code than the
// binary — and then told the operator to run `launchctl kickstart -k`. That
// is the one command guaranteed to fail here: version skew means the binary
// was REPLACED, and launchd pins a job's code identity at bootstrap, so
// kickstarting the old registration makes the kernel SIGKILL the new binary
// at exec (mache-706d8f, diagnosed from a live crash report). The advice was
// wrong every single time it was shown.
func TestCheckVersionSkew_DoesNotRecommendTheCommandThatFails(t *testing.T) {
	// checkVersionSkew refuses to compare on a bare build (no ldflags), which
	// is what `go test` produces — so stand in for a stamped one to reach the
	// branch under test.
	prev := buildVersion
	buildVersion = "v0.9.9-9-gabcdef0"
	t.Cleanup(func() { buildVersion = prev })

	got := checkVersionSkew("0.1.0-old", nil)
	require.Equal(t, statusFail, got.Status, "differing versions must fail")

	assert.NotContains(t, got.Fix, "kickstart",
		"kickstart -k relaunches under the OLD pinned code identity; after a binary "+
			"replacement — which is what version skew MEANS — the kernel kills it")
	assert.Contains(t, got.Fix, "mache daemon restart",
		"the fix must be the verb that reloads the job properly and then verifies it answers")
}
