package graph

import (
	"fmt"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/fixturedb"
)

// countSyncMap reports how many keys a sync.Map holds. The scan caches are
// sync.Maps precisely because FUSE hits them concurrently, so there is no len().
func countSyncMap(m *sync.Map) int {
	n := 0
	m.Range(func(_, _ any) bool { n++; return true })
	return n
}

// TestOpen_PerformsNoEagerScan is a RESOURCE contract, not a behaviour one, and
// it is the reason this file exists separately from the functional tests.
//
// A downstream consumer (modmap) assembles a ~100 GB corpus database by
// ATTACH+INSERT across many projections. That only works because graph.Open is
// SQL-per-call: it opens the file, probes for a `nodes` table, compiles the
// schema levels, and returns. If Open ever grows a bulk load — a scanRoot, a
// DefsMap warm-up, an index build — the assembled path stops fitting in RAM,
// and it does so silently and only at a scale no unit test reaches.
//
// Open's own doc promises this ("Open sidesteps the whole class by not copying
// anything") and nothing enforced it. The three sync.Maps below are populated
// EXCLUSIVELY by scanRoot, so all three being empty after Open is a direct,
// deterministic observation that no scan ran — no timing, no allocation noise.
func TestOpen_PerformsNoEagerScan(t *testing.T) {
	b := fixturedb.New(t, fixturedb.Leyline)
	for i := range 60 {
		id := fixturedb.ConstructID(fmt.Sprintf("pkg%d/file.go/function_declaration_0", i))
		b.Def(fmt.Sprintf("Fn%d", i), id, "function")
	}
	path, _ := b.Build()

	g, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })

	assert.Zero(t, countSyncMap(&g.dirChildren),
		"Open populated the directory cache — something eager-loaded the tree")
	assert.Zero(t, countSyncMap(&g.recordIDs),
		"Open populated the record-id cache — something bulk-read the records")
	assert.Zero(t, countSyncMap(&g.scanOnce),
		"Open started a root scan; scanning belongs to EagerScan, which mount calls explicitly")

	// Laziness must not be satisfied by a broken Open: the graph still has to
	// answer. Without this, deleting the whole body of Open would pass above.
	defs := g.LookupDef("Fn7")
	assert.NotEmpty(t, defs, "a lazy graph must still resolve on demand")
}

// TestEagerScan_IsANoOpOnTheNodesTablePath records a fact that surprised this
// test into existence, and that ADR-0027 gets wrong.
//
// EagerScan exists so a FUSE callback never blocks on a cold scan (fuse-t's
// NFS transport times out past 2s). But it returns immediately when the
// projection has a `nodes` table — which is what `mache build` produces. So
// for every modern projection, mount's EagerScan call does nothing, and the
// scanRoot machinery serves only the legacy schema-projected path.
//
// This is pinned rather than fixed because the no-op is correct: an indexed
// nodes table needs no pre-scan. What is NOT correct is ADR-0027 listing
// EagerScan as a live capability axis worth ablating — on the fast path there
// is nothing to ablate. Recorded on mache-40faff.
func TestEagerScan_IsANoOpOnTheNodesTablePath(t *testing.T) {
	b := fixturedb.New(t, fixturedb.Leyline)
	b.Def("Only", "pkg/file.go/function_declaration_0", "function")
	path, _ := b.Build()

	g, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })

	require.True(t, g.useNodesTable, "mache build produces a nodes table; this test is about that path")
	require.NoError(t, g.EagerScan())
	assert.Zero(t, countSyncMap(&g.scanOnce),
		"EagerScan short-circuits on the nodes-table path — if this ever populates, mount's cost model changed")
}

// TestOpen_CostDoesNotScaleWithProjectionSize is the assertion that actually
// protects the ~100 GB assembled corpus, and it catches eager loading the
// emptiness checks cannot: a DefsMap warm-up, an index build, a bulk read into
// the reader's cache. None of those touch scanOnce/dirChildren.
//
// It asserts on the ABSOLUTE DELTA between two projection sizes, not a ratio.
// A ratio bound fails here: sql.Open's fixed cost is ~240 KB and swamps it. An
// earlier version compared large < small*8 and PASSED with an eager
// g.DefsMap() spliced into Open, because at 400 defs the copy hid inside the
// fixed cost. Measured separately, the signal is not subtle:
//
//	lazy Open      200 -> 2000 defs :  delta ~   256 B   (noise)
//	eager DefsMap  200 -> 2000 defs :  delta ~ 598,000 B
//
// so a 100 KB ceiling sits ~390x above the real measurement and ~6x below the
// cheapest regression it must catch.
func TestOpen_CostDoesNotScaleWithProjectionSize(t *testing.T) {
	openCost := func(defs int) uint64 {
		b := fixturedb.New(t, fixturedb.Leyline)
		for i := range defs {
			id := fixturedb.ConstructID(fmt.Sprintf("pkg%d/file.go/function_declaration_0", i))
			b.Def(fmt.Sprintf("Fn%d", i), id, "function")
		}
		path, _ := b.Build()

		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)

		g, err := Open(path)
		require.NoError(t, err)
		runtime.ReadMemStats(&after)
		t.Cleanup(func() { _ = g.Close() })

		if after.HeapAlloc < before.HeapAlloc {
			return 0 // a GC landed mid-measurement
		}
		return after.HeapAlloc - before.HeapAlloc
	}

	small := openCost(200)
	large := openCost(2000) // 10x the projection

	if small == 0 || large == 0 {
		t.Skip("measurement raced a GC; the emptiness assertions still cover the scan path")
	}
	delta := int64(large) - int64(small)
	t.Logf("heap during Open: 200 defs = %d B, 2000 defs = %d B, delta = %d B", small, large, delta)

	const ceiling = 100 << 10
	assert.Less(t, delta, int64(ceiling),
		"Open's cost grew with projection size — something began bulk-loading, and the assembled ~100GB corpus path cannot survive that")
}
