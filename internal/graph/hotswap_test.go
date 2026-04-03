package graph

import (
	"io/fs"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// HotSwapGraph tests — Swap-specific behavior only.
// Graph interface delegation is covered by TestHotSwapGraph_GraphSuite
// in graph_suite_test.go. These test what the suite can't: swap semantics,
// Close-on-swap, and concurrent swap safety.
// ---------------------------------------------------------------------------

func TestHotSwapGraph_Swap_ReplacesGraph(t *testing.T) {
	store1 := memoryStoreFactory(t)
	store2 := NewMemoryStore()
	store2.AddRoot(&Node{
		ID:       "new",
		Mode:     fs.ModeDir,
		Children: []string{"new/file"},
	})
	store2.AddNode(&Node{ID: "new/file", Mode: 0o444, Data: []byte("replaced")})

	h := NewHotSwapGraph(store1)

	_, err := h.GetNode("pkg/auth/source")
	require.NoError(t, err)
	_, err = h.GetNode("new/file")
	assert.ErrorIs(t, err, ErrNotFound)

	h.Swap(store2)

	_, err = h.GetNode("new/file")
	require.NoError(t, err)
	_, err = h.GetNode("pkg/auth/source")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestHotSwapGraph_Swap_ClosesOldGraph(t *testing.T) {
	old := &closableGraph{Graph: NewMemoryStore()}
	h := NewHotSwapGraph(old)

	assert.False(t, old.closed)
	h.Swap(NewMemoryStore())
	assert.True(t, old.closed, "Swap should close the old graph if it implements io.Closer")
}

func TestHotSwapGraph_Swap_NoCloseIfNotCloser(t *testing.T) {
	h := NewHotSwapGraph(NewMemoryStore())
	h.Swap(NewMemoryStore()) // should not panic
}

func TestHotSwapGraph_ConcurrentReadsDuringSwap(t *testing.T) {
	h := NewHotSwapGraph(memoryStoreFactory(t))

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 500 {
				_, _ = h.GetNode("pkg/auth/source")
				_, _ = h.ListChildren("pkg")
				_, _ = h.ListChildStats("pkg")
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 100 {
			h.Swap(memoryStoreFactory(t))
		}
	}()

	wg.Wait()
}
