package lattice

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNextClosure_Textbook(t *testing.T) {
	// 3-object, 3-attribute cross table (8 concepts):
	//      a  b  c
	// 0:   1  1  0
	// 1:   1  0  1
	// 2:   0  1  1
	ctx := NewFormalContext(3, []string{"a", "b", "c"}, [][]bool{
		{true, true, false},
		{true, false, true},
		{false, true, true},
	})

	concepts := NextClosure(ctx)
	require.Len(t, concepts, 8)

	// Verify concepts in lectic order of intent:
	// {}, {c}, {b}, {b,c}, {a}, {a,c}, {a,b}, {a,b,c}
	expected := []struct {
		intent []uint32
		extent []uint32
	}{
		{intent: nil, extent: []uint32{0, 1, 2}},      // ({0,1,2}, {})
		{intent: []uint32{2}, extent: []uint32{1, 2}}, // ({1,2}, {c})
		{intent: []uint32{1}, extent: []uint32{0, 2}}, // ({0,2}, {b})
		{intent: []uint32{1, 2}, extent: []uint32{2}}, // ({2}, {b,c})
		{intent: []uint32{0}, extent: []uint32{0, 1}}, // ({0,1}, {a})
		{intent: []uint32{0, 2}, extent: []uint32{1}}, // ({1}, {a,c})
		{intent: []uint32{0, 1}, extent: []uint32{0}}, // ({0}, {a,b})
		{intent: []uint32{0, 1, 2}, extent: nil},      // ({}, {a,b,c})
	}

	for i, exp := range expected {
		c := concepts[i]
		// Check intent
		if exp.intent == nil {
			assert.True(t, c.Intent.IsEmpty(), "concept %d intent should be empty", i)
		} else {
			got := c.Intent.ToArray()
			assert.Equal(t, exp.intent, got, "concept %d intent mismatch", i)
		}
		// Check extent
		if exp.extent == nil {
			assert.True(t, c.Extent.IsEmpty(), "concept %d extent should be empty", i)
		} else {
			got := c.Extent.ToArray()
			assert.Equal(t, exp.extent, got, "concept %d extent mismatch", i)
		}
	}
}

func TestNextClosure_Homogeneous(t *testing.T) {
	// All objects have the same attributes → 2 concepts: top and bottom
	//      a  b
	// 0:   1  1
	// 1:   1  1
	// 2:   1  1
	ctx := NewFormalContext(3, []string{"a", "b"}, [][]bool{
		{true, true},
		{true, true},
		{true, true},
	})

	concepts := NextClosure(ctx)
	// Top: ({0,1,2}, {a,b}) — all objects have all attributes
	// This is both top and bottom. Only 1 concept.
	require.Len(t, concepts, 1)
	assert.Equal(t, uint64(3), concepts[0].Extent.GetCardinality())
	assert.Equal(t, uint64(2), concepts[0].Intent.GetCardinality())
}

func TestNextClosure_DisjointGroups(t *testing.T) {
	// Two disjoint groups:
	//      a  b  c  d
	// 0:   1  1  0  0
	// 1:   1  1  0  0
	// 2:   0  0  1  1
	// 3:   0  0  1  1
	ctx := NewFormalContext(4, []string{"a", "b", "c", "d"}, [][]bool{
		{true, true, false, false},
		{true, true, false, false},
		{false, false, true, true},
		{false, false, true, true},
	})

	concepts := NextClosure(ctx)
	// Expected concepts:
	// ({0,1,2,3}, {})       — top
	// ({2,3}, {c,d})        — group 2
	// ({2,3}, {c}) → closure gives {c,d}, so {c} not closed separately...
	// Let me verify: {c}' = {2,3}, {2,3}' = {c,d}, so {c}'' = {c,d}. Not a separate concept.
	// ({0,1}, {a,b})        — group 1
	// ({}, {a,b,c,d})       — bottom
	// That's 4 concepts.
	require.Len(t, concepts, 4)

	// Top concept
	assert.True(t, concepts[0].Intent.IsEmpty())
	assert.Equal(t, uint64(4), concepts[0].Extent.GetCardinality())

	// Bottom concept (last in lectic order)
	last := concepts[len(concepts)-1]
	assert.True(t, last.Extent.IsEmpty())
	assert.Equal(t, uint64(4), last.Intent.GetCardinality())
}

func TestNextClosure_SingleObject(t *testing.T) {
	// Degenerate: 1 object, 2 attributes
	//      a  b
	// 0:   1  1
	ctx := NewFormalContext(1, []string{"a", "b"}, [][]bool{
		{true, true},
	})

	concepts := NextClosure(ctx)
	// Only one concept: ({0}, {a,b})
	require.Len(t, concepts, 1)
	assert.Equal(t, uint64(1), concepts[0].Extent.GetCardinality())
	assert.Equal(t, uint64(2), concepts[0].Intent.GetCardinality())
}
