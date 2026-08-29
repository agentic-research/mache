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
		assert.Contains(t, got.Fix, "mache.log",
			"the fix must name the actual log path, not a generic phrase to go hunt for")
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
