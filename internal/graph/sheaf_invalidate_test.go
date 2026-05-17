package graph

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Mock SheafBackend
// ---------------------------------------------------------------------------

type mockSheafBackend struct {
	mu       sync.Mutex
	calls    []int // region IDs passed to Invalidate
	response []int // affected region IDs to return
	err      error // error to return
}

func (m *mockSheafBackend) Invalidate(regionID int) ([]int, error) {
	m.mu.Lock()
	m.calls = append(m.calls, regionID)
	resp, err := m.response, m.err
	m.mu.Unlock()
	return resp, err
}

// Calls returns a snapshot of the recorded region IDs. Use this in
// assertions instead of touching .calls directly so the mutex protects
// concurrent test usage.
func (m *mockSheafBackend) Calls() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]int, len(m.calls))
	copy(out, m.calls)
	return out
}

// ---------------------------------------------------------------------------
// Mock Graph for tracking Invalidate calls
// ---------------------------------------------------------------------------

type mockGraph struct {
	mu          sync.Mutex
	invalidated []string
}

func (m *mockGraph) GetNode(id string) (*Node, error)                             { return nil, ErrNotFound }
func (m *mockGraph) ListChildren(id string) ([]string, error)                     { return nil, nil }
func (m *mockGraph) ListChildStats(id string) ([]NodeStat, error)                 { return nil, nil }
func (m *mockGraph) ReadContent(id string, buf []byte, offset int64) (int, error) { return 0, nil }
func (m *mockGraph) GetCallers(token string) ([]*Node, error)                     { return nil, nil }
func (m *mockGraph) GetCallees(id string) ([]*Node, error)                        { return nil, nil }
func (m *mockGraph) Act(id, action, payload string) (*ActionResult, error) {
	return nil, ErrActNotSupported
}

func (m *mockGraph) Invalidate(id string) {
	m.mu.Lock()
	m.invalidated = append(m.invalidated, id)
	m.mu.Unlock()
}

// Invalidated returns a snapshot of the recorded node IDs. Use this in
// assertions instead of touching .invalidated directly so the mutex
// protects concurrent test usage.
func (m *mockGraph) Invalidated() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.invalidated))
	copy(out, m.invalidated)
	return out
}

// resetInvalidated clears the recorded list. Test helper for cases that
// reset between phases (e.g. before/after a hot-swap).
func (m *mockGraph) resetInvalidated() {
	m.mu.Lock()
	m.invalidated = nil
	m.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Nil safety
// ---------------------------------------------------------------------------

func TestSheafInvalidator_NilIsNoOp(t *testing.T) {
	var si *SheafInvalidator
	count := si.InvalidateWithCascade("anything", nil)
	assert.Equal(t, 0, count)
}

func TestSheafInvalidator_NilGraphIsNoOp(t *testing.T) {
	si := NewSheafInvalidator(nil, nil, nil)
	count := si.InvalidateWithCascade("anything", nil)
	assert.Equal(t, 0, count)
}

// ---------------------------------------------------------------------------
// Fallback: no sheaf backend → single node invalidation
// ---------------------------------------------------------------------------

func TestSheafInvalidator_FallbackWithoutSheaf(t *testing.T) {
	g := &mockGraph{}
	si := NewSheafInvalidator(g, nil, nil)

	count := si.InvalidateWithCascade("node/a", nil)
	assert.Equal(t, 1, count)
	assert.Equal(t, []string{"node/a"}, g.invalidated)
}

func TestSheafInvalidator_FallbackWithoutCommunityResult(t *testing.T) {
	g := &mockGraph{}
	backend := &mockSheafBackend{response: []int{0}}
	si := NewSheafInvalidator(g, backend, nil)

	count := si.InvalidateWithCascade("node/a", nil)
	assert.Equal(t, 1, count)
	assert.Equal(t, []string{"node/a"}, g.invalidated)
	assert.Empty(t, backend.calls, "should not call sheaf without community result")
}

// ---------------------------------------------------------------------------
// Cascade: node in community → daemon returns affected → invalidate all
// ---------------------------------------------------------------------------

func TestSheafInvalidator_CascadesAcrossRegions(t *testing.T) {
	g := &mockGraph{}
	backend := &mockSheafBackend{
		response: []int{0, 1}, // both regions affected
	}

	cr := &CommunityResult{
		Communities: []Community{
			{ID: 0, Members: []string{"a1", "a2"}},
			{ID: 1, Members: []string{"b1", "b2"}},
		},
		Membership: map[string]int{
			"a1": 0, "a2": 0,
			"b1": 1, "b2": 1,
		},
	}

	si := NewSheafInvalidator(g, backend, cr)
	count := si.InvalidateWithCascade("a1", nil)

	// Should have called sheaf with region 0 (a1's community).
	require.Len(t, backend.calls, 1)
	assert.Equal(t, 0, backend.calls[0])

	// Should have invalidated all 4 nodes (both regions affected).
	assert.Equal(t, 4, count)
	assert.Len(t, g.invalidated, 4)
	assert.Contains(t, g.invalidated, "a1")
	assert.Contains(t, g.invalidated, "a2")
	assert.Contains(t, g.invalidated, "b1")
	assert.Contains(t, g.invalidated, "b2")
}

func TestSheafInvalidator_CascadesOnlyAffectedRegions(t *testing.T) {
	g := &mockGraph{}
	backend := &mockSheafBackend{
		response: []int{0}, // only region 0 affected
	}

	cr := &CommunityResult{
		Communities: []Community{
			{ID: 0, Members: []string{"a1", "a2"}},
			{ID: 1, Members: []string{"b1", "b2"}},
		},
		Membership: map[string]int{
			"a1": 0, "a2": 0,
			"b1": 1, "b2": 1,
		},
	}

	si := NewSheafInvalidator(g, backend, cr)
	count := si.InvalidateWithCascade("a1", nil)

	assert.Equal(t, 2, count)
	assert.Len(t, g.invalidated, 2)
	assert.Contains(t, g.invalidated, "a1")
	assert.Contains(t, g.invalidated, "a2")
	// b1, b2 should NOT be invalidated.
	assert.NotContains(t, g.invalidated, "b1")
	assert.NotContains(t, g.invalidated, "b2")
}

// ---------------------------------------------------------------------------
// Node not in any community → fallback to single invalidation
// ---------------------------------------------------------------------------

func TestSheafInvalidator_UnknownNodeFallsBack(t *testing.T) {
	g := &mockGraph{}
	backend := &mockSheafBackend{response: []int{0}}

	cr := &CommunityResult{
		Communities: []Community{
			{ID: 0, Members: []string{"a1"}},
		},
		Membership: map[string]int{"a1": 0},
	}

	si := NewSheafInvalidator(g, backend, cr)
	count := si.InvalidateWithCascade("unknown_node", nil)

	assert.Equal(t, 1, count)
	assert.Equal(t, []string{"unknown_node"}, g.invalidated)
	assert.Empty(t, backend.calls, "should not call sheaf for unknown node")
}

// ---------------------------------------------------------------------------
// Daemon error → fallback to single invalidation
// ---------------------------------------------------------------------------

func TestSheafInvalidator_DaemonErrorFallsBack(t *testing.T) {
	g := &mockGraph{}
	backend := &mockSheafBackend{
		err: assert.AnError,
	}

	cr := &CommunityResult{
		Communities: []Community{
			{ID: 0, Members: []string{"a1", "a2"}},
		},
		Membership: map[string]int{"a1": 0, "a2": 0},
	}

	si := NewSheafInvalidator(g, backend, cr)
	count := si.InvalidateWithCascade("a1", nil)

	// Should fall back to single invalidation.
	assert.Equal(t, 1, count)
	assert.Equal(t, []string{"a1"}, g.invalidated)
}

// ---------------------------------------------------------------------------
// Daemon returns empty → fallback to single invalidation
// ---------------------------------------------------------------------------

func TestSheafInvalidator_EmptyResponseFallsBack(t *testing.T) {
	g := &mockGraph{}
	backend := &mockSheafBackend{
		response: []int{}, // empty
	}

	cr := &CommunityResult{
		Communities: []Community{
			{ID: 0, Members: []string{"a1", "a2"}},
		},
		Membership: map[string]int{"a1": 0, "a2": 0},
	}

	si := NewSheafInvalidator(g, backend, cr)
	count := si.InvalidateWithCascade("a1", nil)

	assert.Equal(t, 1, count)
	assert.Equal(t, []string{"a1"}, g.invalidated)
}

// ---------------------------------------------------------------------------
// Explicit membership map overrides stored result
// ---------------------------------------------------------------------------

func TestSheafInvalidator_ExplicitMembershipOverridesStored(t *testing.T) {
	g := &mockGraph{}
	backend := &mockSheafBackend{
		response: []int{5}, // region 5
	}

	// Stored result has different membership.
	cr := &CommunityResult{
		Communities: []Community{
			{ID: 0, Members: []string{"a1"}},
		},
		Membership: map[string]int{"a1": 0},
	}

	si := NewSheafInvalidator(g, backend, cr)

	// Explicit membership puts "a1" in region 5.
	explicit := map[string]int{"a1": 5, "x1": 5, "y1": 5}
	count := si.InvalidateWithCascade("a1", explicit)

	require.Len(t, backend.calls, 1)
	assert.Equal(t, 5, backend.calls[0], "should use explicit membership")
	assert.Equal(t, 3, count)
}

func TestSheafInvalidator_ExplicitMembershipWithNilResult(t *testing.T) {
	// Copilot review: explicit membership should enable cascading even when
	// si.result is nil — the caller provided the data we need.
	g := &mockGraph{}
	backend := &mockSheafBackend{
		response: []int{0}, // region 0 affected
	}

	si := NewSheafInvalidator(g, backend, nil) // no stored result

	explicit := map[string]int{"node/a": 0, "node/b": 0}
	count := si.InvalidateWithCascade("node/a", explicit)

	require.Len(t, backend.calls, 1)
	assert.Equal(t, 0, backend.calls[0])
	assert.Equal(t, 2, count, "should cascade using explicit membership despite nil result")
}

// ---------------------------------------------------------------------------
// SetCommunityResult updates the stored result
// ---------------------------------------------------------------------------

func TestSheafInvalidator_SetCommunityResult(t *testing.T) {
	g := &mockGraph{}
	backend := &mockSheafBackend{
		response: []int{0},
	}

	si := NewSheafInvalidator(g, backend, nil)

	// Without community result, falls back.
	count := si.InvalidateWithCascade("a1", nil)
	assert.Equal(t, 1, count)

	// Set community result.
	cr := &CommunityResult{
		Communities: []Community{
			{ID: 0, Members: []string{"a1", "a2"}},
		},
		Membership: map[string]int{"a1": 0, "a2": 0},
	}
	si.SetCommunityResult(cr)

	g.invalidated = nil // reset
	count = si.InvalidateWithCascade("a1", nil)
	assert.Equal(t, 2, count)
	assert.Contains(t, g.invalidated, "a1")
	assert.Contains(t, g.invalidated, "a2")
}

// ---------------------------------------------------------------------------
// Integration with real community detection
// ---------------------------------------------------------------------------

func TestSheafInvalidator_WithRealCommunities(t *testing.T) {
	refs := map[string][]string{
		"alpha": {"a1", "a2", "a3"},
		"beta":  {"a1", "a2", "a3"},
		"gamma": {"b1", "b2", "b3"},
		"delta": {"b1", "b2", "b3"},
	}

	cr := DetectCommunities(refs, 2)
	require.NotNil(t, cr)
	require.Len(t, cr.Communities, 2)

	// Find which community a1 is in.
	a1Region := cr.Membership["a1"]

	g := &mockGraph{}
	backend := &mockSheafBackend{
		response: []int{a1Region}, // only a1's region affected
	}

	si := NewSheafInvalidator(g, backend, cr)
	count := si.InvalidateWithCascade("a1", nil)

	// Should invalidate exactly the 3 nodes in a1's community.
	assert.Equal(t, 3, count)

	// All invalidated nodes should be in the same community.
	for _, nid := range g.invalidated {
		assert.Equal(t, a1Region, cr.Membership[nid])
	}
}

// ---------------------------------------------------------------------------
// Hot-swap + race-safety contracts (mache-c11848)
//
// The watcher goroutine calls InvalidateWithCascade while MCP handlers
// concurrently mutate the invalidator via SetCommunityResult / SetSheaf.
// These tests pin the mutex protection added for the file-watcher wiring.
// ---------------------------------------------------------------------------

// TestSheafInvalidator_HasResult covers the "do I have membership data?"
// accessor the watcher uses to decide whether to short-circuit a cascade
// before even constructing the call. nil-receiver safety is the new
// invariant — without it the cascade path on a nil invalidator would
// nil-deref instead of cleanly degrading to "no cascade".
func TestSheafInvalidator_HasResult(t *testing.T) {
	var nilSi *SheafInvalidator
	assert.False(t, nilSi.HasResult(), "nil receiver must be safe + false")

	si := NewSheafInvalidator(&mockGraph{}, nil, nil)
	assert.False(t, si.HasResult(), "fresh invalidator without result")

	cr := &CommunityResult{
		Communities: []Community{{ID: 1, Members: []string{"a"}}},
		Membership:  map[string]int{"a": 1},
	}
	si.SetCommunityResult(cr)
	assert.True(t, si.HasResult(), "after SetCommunityResult")
}

// TestSheafInvalidator_SetSheaf_Hotswap pins the contract that the
// invalidator can be constructed before the ley-line daemon is reachable
// — the file watcher installs it at serve startup, and the sheaf backend
// gets swapped in later once a connection is established. Pre-swap the
// invalidator must fall back to single-node Graph.Invalidate; post-swap
// the cascade must engage.
func TestSheafInvalidator_SetSheaf_Hotswap(t *testing.T) {
	g := &mockGraph{}
	cr := &CommunityResult{
		Communities: []Community{
			{ID: 1, Members: []string{"a"}},
			{ID: 2, Members: []string{"b"}},
		},
		Membership: map[string]int{"a": 1, "b": 2},
	}
	si := NewSheafInvalidator(g, nil, cr)

	// Pre-swap: no sheaf attached. Must fall back to single-node.
	count := si.InvalidateWithCascade("a", nil)
	assert.Equal(t, 1, count, "no sheaf → single-node")
	assert.Equal(t, []string{"a"}, g.invalidated, "only the requested node")

	// Hot-swap a sheaf backend in mid-flight.
	mock := &mockSheafBackend{response: []int{1, 2}}
	si.SetSheaf(mock)

	g.resetInvalidated()
	count = si.InvalidateWithCascade("a", nil)
	assert.Equal(t, 2, count, "post-swap → full cascade")
	assert.Equal(t, []int{1}, mock.Calls(), "sheaf called with region id")
	assert.ElementsMatch(t, []string{"a", "b"}, g.Invalidated(), "both region members invalidated")

	// Hot-swap back to nil — must return to single-node, not panic.
	si.SetSheaf(nil)
	g.resetInvalidated()
	count = si.InvalidateWithCascade("a", nil)
	assert.Equal(t, 1, count, "nil-swap → fallback")
}

// TestSheafInvalidator_ConcurrentReadWrite is the race-detector gate.
// Without the mutex on (sheaf, result), the watcher goroutine reading
// these fields while the MCP handler swaps them produces the same
// kind of data race the get_communities snapshot fix (PR #380) caught
// — it just hadn't fired yet because there was no concurrent writer.
//
// Run with `go test -race` to actually catch a regression. Without
// -race this only validates that 100 concurrent calls don't panic
// or deadlock, which is weaker but still useful as a smoke test.
func TestSheafInvalidator_ConcurrentReadWrite(t *testing.T) {
	g := &mockGraph{}
	si := NewSheafInvalidator(g, nil, nil)

	const (
		readers    = 8
		writers    = 4
		iterations = 200
	)

	var done sync.WaitGroup
	var readOps int64

	// Reader goroutines hammer InvalidateWithCascade.
	for i := 0; i < readers; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			for j := 0; j < iterations; j++ {
				si.InvalidateWithCascade("a", nil)
				_ = si.HasResult()
				atomic.AddInt64(&readOps, 1)
			}
		}()
	}

	// Writer goroutines hammer SetCommunityResult + SetSheaf.
	for i := 0; i < writers; i++ {
		done.Add(1)
		go func(seed int) {
			defer done.Done()
			for j := 0; j < iterations; j++ {
				cr := &CommunityResult{
					Communities: []Community{{ID: seed, Members: []string{"a"}}},
					Membership:  map[string]int{"a": seed},
				}
				si.SetCommunityResult(cr)
				si.SetSheaf(&mockSheafBackend{response: []int{seed}})
				// And clear them to keep the swap surface honest.
				if j%17 == 0 {
					si.SetSheaf(nil)
				}
				if j%23 == 0 {
					si.SetCommunityResult(nil)
				}
			}
		}(i + 1)
	}

	done.Wait()
	assert.Equal(t, int64(readers*iterations), readOps, "all reads completed")
}
