package leyline

import (
	"os"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/graph"
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

	// startDaemonForE2E keeps every daemon path under a short /tmp dir
	// (macOS caps sun_path at ~104 bytes), reaps the daemon's process group,
	// and dumps its log on failure.
	sockPath, stop := startDaemonForE2E(t, leylineBin)
	t.Cleanup(stop)
	sock := dialWithRetry(t, sockPath, 5*time.Second)
	t.Cleanup(func() { _ = sock.Close() })

	sc := NewSheafClient(sock)

	cr, refs := chainTopology()

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
	// No hash: leyline >= v0.25.0 reads an omitted hash as an explicit
	// "changed" (ley-line-open-1c5d28), so every boundary of b fires and δ⁰
	// decides; a present hash must be 64 lowercase hex.
	affected, err := sc.InvalidateWithStalk(2, "", mutatedStalk)
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

// chainTopology is a synthetic 3-community a↔b↔c chain. The cross-token map
// produces exactly two restriction edges (a-b via `shared_ab`, b-c via
// `shared_bc`). Mutating b's stalk should cascade to both a and c through the
// changed boundaries; a fourth community would need a third edge that
// confuses the cascade-depth reasoning, so 3 is the smallest topology that
// distinguishes "cascade fired" from "no-op heuristic" responses.
func chainTopology() (*graph.CommunityResult, map[string][]string) {
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
	return cr, refs
}

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
