package graph_test

import (
	"fmt"
	"strconv"
	"sync"
	"testing"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// MemoryStore.{Defs,Refs}Map memoization — mache-6b6da6 phase 3.
//
// E2E heap profiling (cmd/all_tools_e2e_test.go + task profile-tools-pprof)
// surfaced these two methods as the dominant allocators in
// `get_impact` (52% heap delta), `get_overview` (32%),
// `get_architecture`, and `get_communities`. Each call previously
// rebuilt the whole map + a fresh slice per entry. The cache
// memoizes the deep-copy snapshot until the next AddDef / AddRef
// / DeleteFileNodes invalidates it.
//
// These tests pin the cache CONTRACT, not the speedup. The e2e
// harness measures the heap improvement empirically. If a future
// refactor re-introduces per-call allocation here, these tests
// fail loudly — that's the CI gate the user asked for.

// mapAddr returns the underlying hmap pointer as a string. Same
// instance → same string; separately-allocated maps → different.
// Go forbids direct map equality so this is the cheap proxy.
func mapAddr[K comparable, V any](m map[K]V) string {
	if m == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%p", m)
}

// TestDefsMap_ReturnsSameInstanceWithoutMutation pins the cache
// hit path: repeated DefsMap calls between AddDef invocations
// must return the SAME map instance, not a re-allocated copy.
// Without the cache this fails because every call re-allocates.
func TestDefsMap_ReturnsSameInstanceWithoutMutation(t *testing.T) {
	s := graph.NewMemoryStore()
	require.NoError(t, s.AddDef("Foo", "pkg/Foo"))
	require.NoError(t, s.AddDef("Bar", "pkg/Bar"))

	first := s.DefsMap()
	require.Len(t, first, 2, "two defs in fixture")

	second := s.DefsMap()
	assert.Equal(t, mapAddr(first), mapAddr(second),
		"DefsMap must return cached snapshot until invalidated; got two distinct map addresses (cache miss on each call)")
}

// TestDefsMap_AddDefInvalidatesCache pins the invalidation path:
// after AddDef, the next DefsMap call must return a fresh
// snapshot reflecting the new state. Snapshots taken before the
// invalidation must remain consistent (callers holding a stale
// reference don't get retroactive mutation).
func TestDefsMap_AddDefInvalidatesCache(t *testing.T) {
	s := graph.NewMemoryStore()
	require.NoError(t, s.AddDef("Foo", "pkg/Foo"))

	first := s.DefsMap()
	require.Len(t, first, 1)
	require.Contains(t, first, "Foo")
	require.NotContains(t, first, "Bar")

	require.NoError(t, s.AddDef("Bar", "pkg/Bar"))
	second := s.DefsMap()

	assert.NotEqual(t, mapAddr(first), mapAddr(second),
		"AddDef must invalidate the DefsMap cache; got the same map address back")
	assert.Contains(t, second, "Foo")
	assert.Contains(t, second, "Bar")
	assert.NotContains(t, first, "Bar",
		"pre-invalidation snapshot must not retroactively gain entries")
}

// TestRefsMap_ReturnsSameInstanceWithoutMutation pins the same
// cache contract for RefsMap (used by community detection).
func TestRefsMap_ReturnsSameInstanceWithoutMutation(t *testing.T) {
	s := graph.NewMemoryStore()
	require.NoError(t, s.AddRef("Foo", "caller/A"))
	require.NoError(t, s.AddRef("Bar", "caller/B"))

	first := s.RefsMap()
	require.Len(t, first, 2)

	second := s.RefsMap()
	assert.Equal(t, mapAddr(first), mapAddr(second),
		"RefsMap must return cached snapshot until invalidated")
}

// TestRefsMap_AddRefInvalidatesCache pins invalidation on AddRef.
func TestRefsMap_AddRefInvalidatesCache(t *testing.T) {
	s := graph.NewMemoryStore()
	require.NoError(t, s.AddRef("Foo", "caller/A"))

	first := s.RefsMap()
	require.Len(t, first, 1)

	require.NoError(t, s.AddRef("Bar", "caller/B"))
	second := s.RefsMap()

	assert.NotEqual(t, mapAddr(first), mapAddr(second),
		"AddRef must invalidate the RefsMap cache")
	assert.Contains(t, second, "Bar")
}

// TestSnapshotCache_ConcurrentReadersGetConsistentSnapshot
// stresses the lazy-rebuild path under concurrency. Multiple
// readers calling DefsMap simultaneously after invalidation must
// all observe a complete (non-torn) snapshot. The double-checked-
// locking in DefsMap is what prevents torn snapshots here; if a
// regression breaks that, this test fails on -race or simply by
// length.
func TestSnapshotCache_ConcurrentReadersGetConsistentSnapshot(t *testing.T) {
	s := graph.NewMemoryStore()
	const seedCount = 100
	for i := 0; i < seedCount; i++ {
		require.NoError(t, s.AddDef("Tok_"+strconv.Itoa(i), "construct/"+strconv.Itoa(i)))
	}

	const readers = 32
	var wg sync.WaitGroup
	wg.Add(readers)
	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			snap := s.DefsMap()
			assert.Len(t, snap, seedCount,
				"concurrent reader saw partial DefsMap snapshot")
		}()
	}
	wg.Wait()
}
