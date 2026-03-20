package leyline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	graph "github.com/agentic-research/mache/internal/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Nil safety — all methods are no-ops on nil SheafClient / nil SocketClient
// ---------------------------------------------------------------------------

func TestSheafClient_NilIsNoOp(t *testing.T) {
	var sc *SheafClient

	err := sc.PushTopology(&graph.CommunityResult{}, nil)
	assert.NoError(t, err)

	ids, err := sc.Invalidate(0)
	assert.NoError(t, err)
	assert.Nil(t, ids)

	d, err := sc.Defect()
	assert.NoError(t, err)
	assert.Equal(t, 0.0, d)

	s, err := sc.Status()
	assert.NoError(t, err)
	assert.Equal(t, SheafStatus{}, s)
}

func TestSheafClient_NilSocketIsNoOp(t *testing.T) {
	sc := NewSheafClient(nil)

	err := sc.PushTopology(&graph.CommunityResult{}, nil)
	assert.NoError(t, err)

	ids, err := sc.Invalidate(0)
	assert.NoError(t, err)
	assert.Nil(t, ids)
}

// ---------------------------------------------------------------------------
// hashMembers
// ---------------------------------------------------------------------------

func TestHashMembers_Deterministic(t *testing.T) {
	h1 := hashMembers([]string{"b", "a", "c"})
	h2 := hashMembers([]string{"c", "a", "b"})
	assert.Equal(t, h1, h2, "hash should be order-independent")

	// Verify it's a valid hex-encoded SHA-256.
	raw, err := hex.DecodeString(h1)
	require.NoError(t, err)
	assert.Len(t, raw, sha256.Size)
}

func TestHashMembers_DifferentSets(t *testing.T) {
	h1 := hashMembers([]string{"a", "b"})
	h2 := hashMembers([]string{"a", "c"})
	assert.NotEqual(t, h1, h2)
}

// ---------------------------------------------------------------------------
// buildRegions
// ---------------------------------------------------------------------------

func TestBuildRegions(t *testing.T) {
	cr := &graph.CommunityResult{
		Communities: []graph.Community{
			{ID: 0, Members: []string{"x", "y"}},
			{ID: 1, Members: []string{"a", "b", "c"}},
		},
	}

	regions := buildRegions(cr)
	require.Len(t, regions, 2)
	assert.Equal(t, 0, regions[0].ID)
	assert.Equal(t, 1, regions[1].ID)
	assert.Equal(t, hashMembers([]string{"x", "y"}), regions[0].Hash)
	assert.Equal(t, hashMembers([]string{"a", "b", "c"}), regions[1].Hash)
}

// ---------------------------------------------------------------------------
// buildRestrictions
// ---------------------------------------------------------------------------

func TestBuildRestrictions_NoCrossLinks(t *testing.T) {
	cr := &graph.CommunityResult{
		Communities: []graph.Community{
			{ID: 0, Members: []string{"a", "b"}},
			{ID: 1, Members: []string{"c", "d"}},
		},
		Membership: map[string]int{"a": 0, "b": 0, "c": 1, "d": 1},
	}

	// Tokens only reference nodes within the same community.
	refs := map[string][]string{
		"t1": {"a", "b"},
		"t2": {"c", "d"},
	}

	restrictions := buildRestrictions(cr, refs)
	assert.Empty(t, restrictions)
}

func TestBuildRestrictions_WithCrossLink(t *testing.T) {
	cr := &graph.CommunityResult{
		Communities: []graph.Community{
			{ID: 0, Members: []string{"a", "b"}},
			{ID: 1, Members: []string{"c", "d"}},
		},
		Membership: map[string]int{"a": 0, "b": 0, "c": 1, "d": 1},
	}

	// "bridge" crosses community 0 and 1.
	refs := map[string][]string{
		"t1":     {"a", "b"},
		"t2":     {"c", "d"},
		"bridge": {"a", "c"},
	}

	restrictions := buildRestrictions(cr, refs)
	require.Len(t, restrictions, 1)
	assert.Equal(t, 0, restrictions[0].A)
	assert.Equal(t, 1, restrictions[0].B)
	assert.Greater(t, restrictions[0].CoChangeRate, 0.0)
	assert.NotEmpty(t, restrictions[0].BoundaryHash)
}

func TestBuildRestrictions_NilRefs(t *testing.T) {
	cr := &graph.CommunityResult{
		Communities: []graph.Community{{ID: 0, Members: []string{"a"}}},
		Membership:  map[string]int{"a": 0},
	}
	restrictions := buildRestrictions(cr, nil)
	assert.Nil(t, restrictions)
}

func TestBuildRestrictions_ThreeCommunities(t *testing.T) {
	cr := &graph.CommunityResult{
		Communities: []graph.Community{
			{ID: 0, Members: []string{"a"}},
			{ID: 1, Members: []string{"b"}},
			{ID: 2, Members: []string{"c"}},
		},
		Membership: map[string]int{"a": 0, "b": 1, "c": 2},
	}

	// One token spans all three communities.
	refs := map[string][]string{
		"shared": {"a", "b", "c"},
	}

	restrictions := buildRestrictions(cr, refs)
	// Should have 3 edges: (0,1), (0,2), (1,2)
	require.Len(t, restrictions, 3)

	pairs := make([][2]int, len(restrictions))
	for i, r := range restrictions {
		pairs[i] = [2]int{r.A, r.B}
	}
	assert.Contains(t, pairs, [2]int{0, 1})
	assert.Contains(t, pairs, [2]int{0, 2})
	assert.Contains(t, pairs, [2]int{1, 2})
}

// ---------------------------------------------------------------------------
// parseIntSlice
// ---------------------------------------------------------------------------

func TestParseIntSlice(t *testing.T) {
	result := parseIntSlice([]any{0.0, 1.0, 5.0})
	assert.Equal(t, []int{0, 1, 5}, result)
}

func TestParseIntSlice_Nil(t *testing.T) {
	assert.Nil(t, parseIntSlice(nil))
	assert.Nil(t, parseIntSlice("not an array"))
}

// ---------------------------------------------------------------------------
// Integration: PushTopology via mock server
// ---------------------------------------------------------------------------

func TestPushTopology_SendsCorrectOp(t *testing.T) {
	var captured map[string]any
	sockPath := mockServer(t, func(req map[string]any) map[string]any {
		captured = req
		return map[string]any{"ok": true}
	})

	sock, err := DialSocket(sockPath)
	require.NoError(t, err)
	defer sock.Close() //nolint:errcheck

	sc := NewSheafClient(sock)
	cr := &graph.CommunityResult{
		Communities: []graph.Community{
			{ID: 0, Members: []string{"a", "b"}},
			{ID: 1, Members: []string{"c", "d"}},
		},
		Membership: map[string]int{"a": 0, "b": 0, "c": 1, "d": 1},
	}
	refs := map[string][]string{
		"bridge": {"a", "c"},
	}

	err = sc.PushTopology(cr, refs)
	require.NoError(t, err)
	require.NotNil(t, captured)

	assert.Equal(t, "sheaf_set_topology", captured["op"])

	// Verify regions are present.
	regionsRaw, ok := captured["regions"].([]any)
	require.True(t, ok)
	assert.Len(t, regionsRaw, 2)

	// Verify restrictions are present.
	restrictionsRaw, ok := captured["restrictions"].([]any)
	require.True(t, ok)
	assert.Len(t, restrictionsRaw, 1)
}

func TestPushTopology_NilCommunityResult(t *testing.T) {
	sc := NewSheafClient(nil)
	assert.NoError(t, sc.PushTopology(nil, nil))
}

// ---------------------------------------------------------------------------
// Integration: Invalidate via mock server
// ---------------------------------------------------------------------------

func TestInvalidate_ReturnsAffectedRegions(t *testing.T) {
	sockPath := mockServer(t, func(req map[string]any) map[string]any {
		assert.Equal(t, "sheaf_invalidate", req["op"])
		return map[string]any{
			"invalidated": []any{0.0, 1.0, 3.0},
			"count":       3.0,
			"generation":  2.0,
		}
	})

	sock, err := DialSocket(sockPath)
	require.NoError(t, err)
	defer sock.Close() //nolint:errcheck

	sc := NewSheafClient(sock)
	affected, err := sc.Invalidate(0)
	require.NoError(t, err)
	assert.Equal(t, []int{0, 1, 3}, affected)
}

func TestInvalidate_DaemonError(t *testing.T) {
	sockPath := mockServer(t, func(req map[string]any) map[string]any {
		return map[string]any{"error": "region 99 not found"}
	})

	sock, err := DialSocket(sockPath)
	require.NoError(t, err)
	defer sock.Close() //nolint:errcheck

	sc := NewSheafClient(sock)
	_, err = sc.Invalidate(99)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "region 99 not found")
}

// ---------------------------------------------------------------------------
// Integration: Defect via mock server
// ---------------------------------------------------------------------------

func TestDefect_ReturnsScore(t *testing.T) {
	sockPath := mockServer(t, func(req map[string]any) map[string]any {
		assert.Equal(t, "sheaf_defect", req["op"])
		return map[string]any{
			"defect":     0.42,
			"generation": 5.0,
			"valid":      10.0,
			"total":      15.0,
		}
	})

	sock, err := DialSocket(sockPath)
	require.NoError(t, err)
	defer sock.Close() //nolint:errcheck

	sc := NewSheafClient(sock)
	d, err := sc.Defect()
	require.NoError(t, err)
	assert.InDelta(t, 0.42, d, 0.001)
}

// ---------------------------------------------------------------------------
// Integration: Status via mock server
// ---------------------------------------------------------------------------

func TestStatus_ParsesFullResponse(t *testing.T) {
	sockPath := mockServer(t, func(req map[string]any) map[string]any {
		assert.Equal(t, "sheaf_status", req["op"])
		return map[string]any{
			"generation": 7.0,
			"valid":      20.0,
			"total":      25.0,
			"defect":     0.2,
		}
	})

	sock, err := DialSocket(sockPath)
	require.NoError(t, err)
	defer sock.Close() //nolint:errcheck

	sc := NewSheafClient(sock)
	s, err := sc.Status()
	require.NoError(t, err)
	assert.Equal(t, uint64(7), s.Generation)
	assert.Equal(t, 20, s.Valid)
	assert.Equal(t, 25, s.Total)
	assert.InDelta(t, 0.2, s.Defect, 0.001)
}

// ---------------------------------------------------------------------------
// JSON serialization of topology (verifies the wire format)
// ---------------------------------------------------------------------------

func TestTopologyJSON_WireFormat(t *testing.T) {
	cr := &graph.CommunityResult{
		Communities: []graph.Community{
			{ID: 0, Members: []string{"a", "b"}},
		},
		Membership: map[string]int{"a": 0, "b": 0},
	}

	regions := buildRegions(cr)
	data, err := json.Marshal(regions)
	require.NoError(t, err)

	var parsed []map[string]any
	require.NoError(t, json.Unmarshal(data, &parsed))
	assert.Len(t, parsed, 1)
	assert.Equal(t, 0.0, parsed[0]["id"])

	// Hash should be hex-encoded SHA-256.
	hashStr, ok := parsed[0]["hash"].(string)
	require.True(t, ok)
	raw, err := hex.DecodeString(hashStr)
	require.NoError(t, err)
	assert.Len(t, raw, sha256.Size)
}

func TestRestrictionJSON_WireFormat(t *testing.T) {
	r := restriction{
		A:            0,
		B:            1,
		BoundaryHash: strings.Repeat("ab", 32),
		CoChangeRate: 0.75,
	}
	data, err := json.Marshal(r)
	require.NoError(t, err)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(data, &parsed))
	assert.Equal(t, 0.0, parsed["a"])
	assert.Equal(t, 1.0, parsed["b"])
	assert.Equal(t, 0.75, parsed["co_change_rate"])
	assert.Equal(t, strings.Repeat("ab", 32), parsed["boundary_hash"])
}

// ---------------------------------------------------------------------------
// buildRestrictions co_change_rate scaling
// ---------------------------------------------------------------------------

func TestBuildRestrictions_CoChangeRateScales(t *testing.T) {
	cr := &graph.CommunityResult{
		Communities: []graph.Community{
			{ID: 0, Members: []string{"a1", "a2", "a3"}},
			{ID: 1, Members: []string{"b1", "b2", "b3"}},
		},
		Membership: map[string]int{
			"a1": 0, "a2": 0, "a3": 0,
			"b1": 1, "b2": 1, "b3": 1,
		},
	}

	// One token shared by 1 node from each community.
	weakRefs := map[string][]string{
		"weak": {"a1", "b1"},
	}
	weakR := buildRestrictions(cr, weakRefs)
	require.Len(t, weakR, 1)

	// Many tokens shared by many nodes from each community.
	strongRefs := map[string][]string{
		"s1": {"a1", "a2", "b1", "b2"},
		"s2": {"a1", "a3", "b1", "b3"},
		"s3": {"a2", "a3", "b2", "b3"},
	}
	strongR := buildRestrictions(cr, strongRefs)
	require.Len(t, strongR, 1)

	assert.Greater(t, strongR[0].CoChangeRate, weakR[0].CoChangeRate,
		"more cross-links should produce a higher co_change_rate")
}

// ---------------------------------------------------------------------------
// hashMembers edge cases
// ---------------------------------------------------------------------------

func TestHashMembers_Empty(t *testing.T) {
	h := hashMembers(nil)
	// SHA-256 of empty input.
	expected := sha256.Sum256(nil)
	assert.Equal(t, hex.EncodeToString(expected[:]), h)
}

func TestHashMembers_SingleMember(t *testing.T) {
	h := hashMembers([]string{"only"})
	expected := sha256.Sum256([]byte("only"))
	assert.Equal(t, hex.EncodeToString(expected[:]), h)
}

// ---------------------------------------------------------------------------
// buildRestrictions deterministic ordering
// ---------------------------------------------------------------------------

func TestBuildRestrictions_DeterministicOrder(t *testing.T) {
	cr := &graph.CommunityResult{
		Communities: []graph.Community{
			{ID: 0, Members: []string{"a"}},
			{ID: 1, Members: []string{"b"}},
			{ID: 2, Members: []string{"c"}},
		},
		Membership: map[string]int{"a": 0, "b": 1, "c": 2},
	}
	refs := map[string][]string{
		"x": {"a", "b", "c"},
	}

	// Run multiple times to ensure ordering is stable.
	var prev []restriction
	for i := 0; i < 5; i++ {
		r := buildRestrictions(cr, refs)
		if prev != nil {
			for j := range r {
				assert.Equal(t, prev[j].A, r[j].A)
				assert.Equal(t, prev[j].B, r[j].B)
			}
		}
		prev = r
	}

	// Verify sorted by (A, B).
	for i := 1; i < len(prev); i++ {
		assert.True(t, prev[i-1].A < prev[i].A ||
			(prev[i-1].A == prev[i].A && prev[i-1].B <= prev[i].B))
	}
}

// ---------------------------------------------------------------------------
// Integration: full round-trip with community detection
// ---------------------------------------------------------------------------

func TestSheafClient_EndToEnd_WithCommunities(t *testing.T) {
	// Detect communities, push topology, invalidate a region.
	refs := map[string][]string{
		"alpha":  {"a1", "a2", "a3"},
		"beta":   {"a1", "a2", "a3"},
		"gamma":  {"b1", "b2", "b3"},
		"delta":  {"b1", "b2", "b3"},
		"bridge": {"a1", "b1"},
	}

	cr := graph.DetectCommunities(refs, 2)
	require.NotNil(t, cr)
	require.GreaterOrEqual(t, len(cr.Communities), 2)

	var topologyReq map[string]any
	var invalidateReq map[string]any
	callCount := 0

	sockPath := mockServer(t, func(req map[string]any) map[string]any {
		callCount++
		switch req["op"] {
		case "sheaf_set_topology":
			topologyReq = req
			return map[string]any{"ok": true}
		case "sheaf_invalidate":
			invalidateReq = req
			return map[string]any{
				"invalidated": []any{0.0, 1.0},
				"count":       2.0,
				"generation":  1.0,
			}
		default:
			return map[string]any{"error": "unexpected op"}
		}
	})

	sock, err := DialSocket(sockPath)
	require.NoError(t, err)
	defer sock.Close() //nolint:errcheck

	sc := NewSheafClient(sock)

	// Push topology.
	err = sc.PushTopology(cr, refs)
	require.NoError(t, err)
	require.NotNil(t, topologyReq)

	regionsRaw := topologyReq["regions"].([]any)
	assert.GreaterOrEqual(t, len(regionsRaw), 2)

	// Invalidate a region.
	affected, err := sc.Invalidate(0)
	require.NoError(t, err)
	assert.Equal(t, []int{0, 1}, affected)
	require.NotNil(t, invalidateReq)

	assert.Equal(t, 2, callCount)
}

// ---------------------------------------------------------------------------
// Helpers for verifying hash consistency with community detection
// ---------------------------------------------------------------------------

func TestBuildRegions_HashMatchesCommunityMembers(t *testing.T) {
	refs := map[string][]string{
		"t1": {"a", "b", "c"},
		"t2": {"d", "e", "f"},
	}
	cr := graph.DetectCommunities(refs, 2)
	require.GreaterOrEqual(t, len(cr.Communities), 1)

	regions := buildRegions(cr)
	for i, r := range regions {
		members := cr.Communities[i].Members
		sorted := make([]string, len(members))
		copy(sorted, members)
		sort.Strings(sorted)

		h := sha256.New()
		for j, m := range sorted {
			if j > 0 {
				h.Write([]byte("\n"))
			}
			h.Write([]byte(m))
		}
		expected := hex.EncodeToString(h.Sum(nil))
		assert.Equal(t, expected, r.Hash, "region %d hash should match manual computation", i)
	}
}
