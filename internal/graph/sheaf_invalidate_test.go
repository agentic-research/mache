package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Mock SheafBackend
// ---------------------------------------------------------------------------

type mockSheafBackend struct {
	calls    []int // region IDs passed to Invalidate
	response []int // affected region IDs to return
	err      error // error to return
}

func (m *mockSheafBackend) Invalidate(regionID int) ([]int, error) {
	m.calls = append(m.calls, regionID)
	return m.response, m.err
}

// ---------------------------------------------------------------------------
// Mock Graph for tracking Invalidate calls
// ---------------------------------------------------------------------------

type mockGraph struct {
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
	m.invalidated = append(m.invalidated, id)
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
