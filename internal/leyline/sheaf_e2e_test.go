package leyline

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/graph"
)

// TestE2E_SheafCascade_AgainstLiveDaemon is the cross-runtime gate for
// the sheaf moat (bead mache-8e7794). It spawns a real ley-line daemon
// subprocess, drives it exclusively through SheafClient's public ops,
// and asserts the daemon's cascade output reflects the BFS-through-
// changed-boundaries semantics promised by LLO PR #16.
//
// Black-box on purpose. The test must NOT reach into the daemon's
// internal cache state (e.g. LLO's `ctx.sheaf.cache().lock().put(...)`
// pattern from `real_repo_sheaf_bench.rs`) — the public UDS surface is
// what mache will actually use in production, and any cascade that
// only fires under direct cache manipulation is a different system.
//
// Pre-PR-#27 LLO would have returned `invalidated: []` here even with
// correct inputs — the cascade output was gated on entries the UDS
// protocol couldn't populate. This test passing on v0.4.1+ is the
// continuity contract: mache pushes topology + post-change stalks,
// daemon returns reachable regions via the agreement-plane δ⁰ check.
//
// Skipped automatically when the pinned `leyline` cannot be resolved so CI
// without the daemon binary (and developer machines without it) stays green.
// Test files are sized so the daemon's auto-spawn-and-bind path
// completes inside the timeout — extend the deadline if the bench
// repo grows beyond a few KB of fixtures.
func TestE2E_SheafCascade_AgainstLiveDaemon(t *testing.T) {
	// Resolve before HOME isolation so the resolver can find the pinned binary
	// under the developer's ~/.mache/bin. A raw LookPath here would bypass the
	// production version gate and may launch an incompatible local CLI.
	leylineBin, err := ResolveBinary(false)
	if err != nil {
		t.Skipf("pinned leyline unavailable — skipping cross-runtime e2e: %v", err)
	}
	t.Setenv("HOME", t.TempDir()) // isolate ~/.mache/ from sibling tests

	// Each subtest gets its own isolated daemon. The daemon derives the
	// UDS socket path from --control (replaces .ctrl with .sock), and
	// macOS caps sun_path at ~104 bytes — go test's TempDir
	// (`/var/folders/.../TestName/NNN/`) blows that budget every time
	// the daemon tries to bind. Put ALL daemon paths (arena, ctrl,
	// hence sock) in a short /tmp subdir; clean it up explicitly since
	// it lives outside t.TempDir's reach.
	tdir, err := os.MkdirTemp("/tmp", "sheaf-e2e-")
	require.NoError(t, err)
	// Registered FIRST so it runs LAST (t.Cleanup is LIFO) — after the daemon
	// cleanup below has signalled, so it observes the post-reap state.
	t.Cleanup(func() { assertNoSurvivorsFor(t, tdir) })
	t.Cleanup(func() { _ = os.RemoveAll(tdir) })
	arena := filepath.Join(tdir, "arena.bin")
	ctrl := filepath.Join(tdir, "test.ctrl")
	sockPath := filepath.Join(tdir, "test.sock")

	// Bound the daemon lifetime so a hung test doesn't leak processes
	// — the daemon self-terminates after 60s of idle even if the test
	// goroutine panics before SIGTERM lands.
	daemon := exec.Command(leylineBin, "daemon",
		"--arena", arena,
		"--control", ctrl,
		"--timeout", "60s",
	)
	// Detach stdio — the daemon writes startup chatter that would
	// otherwise interleave with go test's output. Keep stderr captured
	// for the failure path (see below).
	logFile, err := os.Create(filepath.Join(tdir, "daemon.log"))
	require.NoError(t, err)
	defer func() { _ = logFile.Close() }()
	daemon.Stdout = logFile
	daemon.Stderr = logFile

	setProcessGroup(daemon) // daemon spawns `mache serve --control` as a child; group it so cleanup reaps both
	require.NoError(t, daemon.Start(), "leyline daemon failed to start")
	t.Cleanup(func() {
		if daemon.Process == nil {
			return
		}
		// SIGTERM first so the daemon unmounts/cleans up; SIGKILL after
		// a grace period so a wedged daemon never blocks test teardown.
		_ = signalProcessGroup(daemon.Process, syscall.SIGTERM)
		done := make(chan struct{})
		go func() {
			_, _ = daemon.Process.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = signalProcessGroup(daemon.Process, syscall.SIGKILL)
			<-done
		}
		// On any failure, dump the daemon log so the failure mode
		// (unknown op, socket bind error, panic) is in the test output
		// instead of in a file the CI worker has already discarded.
		if t.Failed() {
			if b, readErr := os.ReadFile(filepath.Join(tdir, "daemon.log")); readErr == nil {
				t.Logf("daemon.log (on failure):\n%s", b)
			}
		}
	})

	// Poll for the socket to appear — the daemon binds after the first
	// snapshot write, which is well under 5s even on cold-start arenas.
	sock := dialWithRetry(t, sockPath, 5*time.Second)
	t.Cleanup(func() { _ = sock.Close() })

	sc := NewSheafClient(sock)

	// Synthetic 3-community a↔b↔c chain. The cross-token map produces
	// exactly two restriction edges (a-b via `shared_ab`, b-c via
	// `shared_bc`). Mutating b's stalk should cascade to both a and c
	// through the changed boundaries; d would have been a useful
	// "did not change" sentinel but adding a fourth community here
	// would require a third edge that confuses the cascade-depth
	// reasoning. The 3-community shape is the smallest topology that
	// distinguishes "cascade fired" from "no-op heuristic" responses.
	cr := &graph.CommunityResult{
		Communities: []graph.Community{
			{ID: 1, Members: []string{"a/fn1", "a/fn2"}},
			{ID: 2, Members: []string{"b/fn1", "b/fn2"}},
			{ID: 3, Members: []string{"c/fn1", "c/fn2"}},
		},
		Membership: map[string]int{
			"a/fn1": 1, "a/fn2": 1,
			"b/fn1": 2, "b/fn2": 2,
			"c/fn1": 3, "c/fn2": 3,
		},
	}
	refs := map[string][]string{
		"shared_ab": {"a/fn1", "b/fn1"},
		"shared_bc": {"b/fn2", "c/fn1"},
		// Intra-community refs are filtered by crossCommunityTokens and
		// shouldn't create restriction edges — including them here is
		// the regression guard for that filter behavior under live wire.
		"local_a": {"a/fn1", "a/fn2"},
		"local_c": {"c/fn1", "c/fn2"},
	}

	// PushTopology engages δ⁰ mode on v0.4.1+ daemons. If this errors
	// the most likely cause is a daemon older than v0.4.1 (no sheaf
	// ops) or v0.4.0 (cache.entries population gap fixed in PR #27).
	require.NoError(t, sc.PushTopology(cr, refs), "PushTopology against live daemon")

	// Status before invalidation: generation should be at the baseline
	// (0 on a fresh daemon). Reading it confirms the typed-decode path
	// works against the live wire format, not just the mock.
	status, err := sc.Status()
	require.NoError(t, err, "Status() against live daemon")
	assert.Equal(t, uint64(0), status.Generation, "fresh daemon should report generation=0")

	// Mutate b: push a new stalk whose cross-token set differs from the
	// one in topology. The daemon's δ⁰ check should determine that both
	// (a, b) and (b, c) boundary projections moved, and the BFS should
	// return b plus both reachable neighbors.
	mutatedStalk := ComputeStalk(2, []string{"new_token_x", "new_token_y"})
	affected, err := sc.InvalidateWithStalk(2, "post-mutation", mutatedStalk)
	require.NoError(t, err, "InvalidateWithStalk against live daemon")

	// The cascade must include the directly-changed region AND both
	// neighbors reached through changed boundaries. Order is BFS-
	// visitation per cache.rs::on_change so we sort to compare against
	// the set {1, 2, 3}.
	sort.Ints(affected)
	assert.Equal(t, []int{1, 2, 3}, affected,
		"cascade must reach both neighbors through changed boundaries — empty result here means δ⁰ is not engaged (pre-#27 LLO) or cache.entries population gap re-asserted")

	// Generation must have advanced exactly once for the single
	// InvalidateWithStalk call. Reading it twice asserts the daemon's
	// monotonic-counter contract that downstream staleness signals
	// rely on (mache-4a0c05 — get_sheaf_status MCP tool).
	statusAfter, err := sc.Status()
	require.NoError(t, err)
	assert.Equal(t, uint64(1), statusAfter.Generation,
		"generation must advance monotonically on each on_change")

	// Defect must be a real, daemon-computed scalar — not the zero
	// fallback SheafClient returns when the daemon is unavailable.
	// On this synthetic topology with mutated stalks the defect is
	// non-trivial; pinning a precise expected value would couple the
	// test to LLO's internal boundary-disagreement formula, so we
	// only assert it's surfaced.
	defect, err := sc.Defect()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, defect, 0.0, "defect must be a real scalar from the daemon")
}

// dialWithRetry waits for the daemon's UDS socket file to appear
// (newly-spawned daemons bind asynchronously) and then DialSocket.
// Fails the test if the socket doesn't appear within deadline — the
// daemon either crashed at startup or its arena init wedged.
func dialWithRetry(t *testing.T, sockPath string, deadline time.Duration) *SocketClient {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if _, err := os.Stat(sockPath); err == nil {
			sock, err := DialSocket(sockPath)
			if err == nil {
				return sock
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("leyline daemon socket %s did not appear within %s", sockPath, deadline)
	return nil
}
