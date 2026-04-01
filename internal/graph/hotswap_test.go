package graph

import (
	"io/fs"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// HotSwapGraph characterization tests
//
// Every Graph method on HotSwapGraph is pure delegation under RLock.
// These tests verify: (1) delegation works, (2) Swap replaces the graph,
// (3) concurrent access doesn't deadlock.
// ---------------------------------------------------------------------------

func newTestStore() *MemoryStore {
	s := NewMemoryStore()
	s.AddRoot(&Node{
		ID:       "pkg",
		Mode:     fs.ModeDir,
		Children: []string{"pkg/main"},
	})
	s.AddNode(&Node{
		ID:       "pkg/main",
		Mode:     fs.ModeDir,
		Children: []string{"pkg/main/source"},
	})
	s.AddNode(&Node{
		ID:   "pkg/main/source",
		Mode: 0o444,
		Data: []byte("package main"),
	})
	return s
}

func TestHotSwapGraph_GetNode(t *testing.T) {
	h := NewHotSwapGraph(newTestStore())
	n, err := h.GetNode("pkg/main/source")
	require.NoError(t, err)
	assert.Equal(t, "pkg/main/source", n.ID)
	assert.Equal(t, []byte("package main"), n.Data)
}

func TestHotSwapGraph_GetNode_NotFound(t *testing.T) {
	h := NewHotSwapGraph(newTestStore())
	_, err := h.GetNode("nonexistent")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestHotSwapGraph_ListChildren(t *testing.T) {
	h := NewHotSwapGraph(newTestStore())
	children, err := h.ListChildren("pkg")
	require.NoError(t, err)
	assert.Equal(t, []string{"pkg/main"}, children)
}

func TestHotSwapGraph_ListChildStats(t *testing.T) {
	h := NewHotSwapGraph(newTestStore())
	stats, err := h.ListChildStats("pkg")
	require.NoError(t, err)
	require.Len(t, stats, 1)
	assert.Equal(t, "pkg/main", stats[0].ID)
	assert.True(t, stats[0].IsDir)
}

func TestHotSwapGraph_ReadContent(t *testing.T) {
	h := NewHotSwapGraph(newTestStore())
	buf := make([]byte, 100)
	n, err := h.ReadContent("pkg/main/source", buf, 0)
	require.NoError(t, err)
	assert.Equal(t, "package main", string(buf[:n]))
}

func TestHotSwapGraph_GetCallers_Empty(t *testing.T) {
	h := NewHotSwapGraph(newTestStore())
	callers, err := h.GetCallers("Println")
	require.NoError(t, err)
	assert.Empty(t, callers)
}

func TestHotSwapGraph_GetCallees_Empty(t *testing.T) {
	h := NewHotSwapGraph(newTestStore())
	callees, err := h.GetCallees("pkg/main")
	require.NoError(t, err)
	assert.Empty(t, callees)
}

func TestHotSwapGraph_Invalidate(t *testing.T) {
	h := NewHotSwapGraph(newTestStore())
	// Should not panic — Invalidate is a no-op on MemoryStore
	h.Invalidate("pkg/main/source")
}

func TestHotSwapGraph_Act(t *testing.T) {
	h := NewHotSwapGraph(newTestStore())
	_, err := h.Act("pkg/main", "click", "")
	assert.ErrorIs(t, err, ErrActNotSupported)
}

// ---------------------------------------------------------------------------
// Swap behavior
// ---------------------------------------------------------------------------

func TestHotSwapGraph_Swap_ReplacesGraph(t *testing.T) {
	store1 := newTestStore()
	store2 := NewMemoryStore()
	store2.AddRoot(&Node{
		ID:       "new",
		Mode:     fs.ModeDir,
		Children: []string{"new/file"},
	})
	store2.AddNode(&Node{ID: "new/file", Mode: 0o444, Data: []byte("replaced")})

	h := NewHotSwapGraph(store1)

	// Before swap: old data visible
	_, err := h.GetNode("pkg/main/source")
	require.NoError(t, err)
	_, err = h.GetNode("new/file")
	assert.ErrorIs(t, err, ErrNotFound)

	h.Swap(store2)

	// After swap: new data visible, old gone
	_, err = h.GetNode("new/file")
	require.NoError(t, err)
	_, err = h.GetNode("pkg/main/source")
	assert.ErrorIs(t, err, ErrNotFound)
}

type closableStore struct {
	*MemoryStore
	closed bool
}

func (c *closableStore) Close() error {
	c.closed = true
	return nil
}

func TestHotSwapGraph_Swap_ClosesOldGraph(t *testing.T) {
	old := &closableStore{MemoryStore: NewMemoryStore()}
	h := NewHotSwapGraph(old)

	assert.False(t, old.closed)
	h.Swap(NewMemoryStore())
	assert.True(t, old.closed, "Swap should close the old graph if it implements io.Closer")
}

func TestHotSwapGraph_Swap_NoCloseIfNotCloser(t *testing.T) {
	// MemoryStore without Close wrapper — Swap should not panic
	h := NewHotSwapGraph(NewMemoryStore())
	h.Swap(NewMemoryStore()) // should not panic
}

// ---------------------------------------------------------------------------
// Concurrency: reads during swap don't deadlock
// ---------------------------------------------------------------------------

func TestHotSwapGraph_ConcurrentReadsDuringSwap(t *testing.T) {
	h := NewHotSwapGraph(newTestStore())

	var wg sync.WaitGroup
	// 10 readers
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 500 {
				_, _ = h.GetNode("pkg/main/source")
				_, _ = h.ListChildren("pkg")
				_, _ = h.ListChildStats("pkg")
			}
		}()
	}
	// 1 writer swapping continuously
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 100 {
			h.Swap(newTestStore())
		}
	}()

	wg.Wait()
}
