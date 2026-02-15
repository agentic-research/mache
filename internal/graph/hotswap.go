package graph

import (
	"sync"
)

// HotSwapGraph is a thread-safe wrapper that allows swapping the underlying graph instance.
type HotSwapGraph struct {
	mu      sync.RWMutex
	current Graph
}

func NewHotSwapGraph(initial Graph) *HotSwapGraph {
	return &HotSwapGraph{current: initial}
}

// Swap atomically replaces the current graph with a new one.
// It closes the old graph (if it implements Closer, though Graph interface doesn't enforce it).
// Ideally, Graph implementations should be safe to close.
func (h *HotSwapGraph) Swap(newGraph Graph) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Optional: Close old graph if possible
	// if closer, ok := h.current.(io.Closer); ok { closer.Close() }
	h.current = newGraph
}

// GetNode delegates to current graph.
func (h *HotSwapGraph) GetNode(id string) (*Node, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.current.GetNode(id)
}

// ListChildren delegates to current graph.
func (h *HotSwapGraph) ListChildren(id string) ([]string, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.current.ListChildren(id)
}

// ReadContent delegates to current graph.
func (h *HotSwapGraph) ReadContent(id string, buf []byte, offset int64) (int, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.current.ReadContent(id, buf, offset)
}

// GetCallers delegates to current graph.
func (h *HotSwapGraph) GetCallers(token string) ([]*Node, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.current.GetCallers(token)
}

// Invalidate delegates to current graph.
func (h *HotSwapGraph) Invalidate(id string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	h.current.Invalidate(id)
}
