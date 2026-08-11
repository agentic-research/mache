package graph

import (
	"io/fs"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Invariant tests — catch structural bugs across all Graph backends.
// These should be added to RunGraphSuite once all backends comply.
// For now they target MemoryStore directly since it has the aliasing bugs.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// ListChildren must not alias internal state (mache-e7bf4d)
//
// FALSIFIABLE: If ListChildren returns the internal slice, sorting the
// result reorders the store's Children. With a defensive copy, the
// store is unaffected.
// ---------------------------------------------------------------------------

func TestMemoryStore_ListChildren_NoAlias(t *testing.T) {
	store := NewMemoryStore()
	store.AddRoot(&Node{
		ID:       "pkg",
		Mode:     fs.ModeDir,
		Children: []string{"pkg/b", "pkg/a", "pkg/c"},
	})
	store.AddNode(&Node{ID: "pkg/a", Mode: fs.ModeDir})
	store.AddNode(&Node{ID: "pkg/b", Mode: fs.ModeDir})
	store.AddNode(&Node{ID: "pkg/c", Mode: fs.ModeDir})

	// Get children
	children, err := store.ListChildren("pkg")
	require.NoError(t, err)
	assert.Equal(t, []string{"pkg/b", "pkg/a", "pkg/c"}, children)

	// Mutate the returned slice
	children[0], children[1] = children[1], children[0]

	// Store's internal order must be unchanged
	again, err := store.ListChildren("pkg")
	require.NoError(t, err)
	assert.Equal(t, []string{"pkg/b", "pkg/a", "pkg/c"}, again,
		"ListChildren returned aliased slice — caller mutation corrupted store state")
}

func TestMemoryStore_ListChildren_RootNoAlias(t *testing.T) {
	store := NewMemoryStore()
	store.AddRoot(&Node{ID: "z", Mode: fs.ModeDir})
	store.AddRoot(&Node{ID: "a", Mode: fs.ModeDir})

	roots, err := store.ListChildren("/")
	require.NoError(t, err)

	// Mutate
	roots[0] = "CORRUPTED"

	// Store's roots must be unchanged
	again, err := store.ListChildren("/")
	require.NoError(t, err)
	assert.NotContains(t, again, "CORRUPTED",
		"ListChildren('/') returned aliased roots slice")
}

// ---------------------------------------------------------------------------
// ShiftOrigins must clamp to 0 on underflow (mache-e7c80c)
//
// FALSIFIABLE: Without the clamp, uint32(int32(5) + (-10)) wraps to
// 4294967291. With the clamp, it stays at 0.
// ---------------------------------------------------------------------------

func TestMemoryStore_ShiftOrigins_UnderflowClamp(t *testing.T) {
	store := NewMemoryStore()

	store.AddNode(&Node{
		ID:   "pkg/Small",
		Mode: 0,
		Data: []byte("x"),
		Origin: &SourceOrigin{
			FilePath:  "/src/main.go",
			StartByte: 5,
			EndByte:   10,
		},
	})

	// Delta larger than StartByte — would wrap uint32 without clamp
	store.ShiftOrigins("/src/main.go", 0, -20)

	n, err := store.GetNode("pkg/Small")
	require.NoError(t, err)

	assert.Equal(t, uint32(0), n.Origin.StartByte,
		"ShiftOrigins underflowed — StartByte should clamp to 0, got %d", n.Origin.StartByte)
	assert.Equal(t, uint32(0), n.Origin.EndByte,
		"ShiftOrigins underflowed — EndByte should clamp to 0, got %d", n.Origin.EndByte)
}

func TestMemoryStore_ShiftOrigins_ExactZero(t *testing.T) {
	store := NewMemoryStore()

	store.AddNode(&Node{
		ID:   "pkg/Exact",
		Mode: 0,
		Origin: &SourceOrigin{
			FilePath:  "/src/main.go",
			StartByte: 10,
			EndByte:   20,
		},
	})

	// Delta exactly cancels StartByte
	store.ShiftOrigins("/src/main.go", 0, -10)

	n, _ := store.GetNode("pkg/Exact")
	assert.Equal(t, uint32(0), n.Origin.StartByte)
	assert.Equal(t, uint32(10), n.Origin.EndByte)
}
