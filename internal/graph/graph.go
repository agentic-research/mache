package graph

import (
	"errors"
	"sync"
)

var ErrNotFound = errors.New("node not found")

// Node is the universal primitive.
// It can be a Directory (if Children is non-empty) or a File (if Properties has "content").
type Node struct {
	ID         string
	Properties map[string][]byte
	Children   []string // IDs of child nodes
}

// Graph is the Read-Only interface for the FUSE layer.
// This allows us to swap the backend later (Memory -> SQLite -> Mmap).
type Graph interface {
	GetNode(id string) (*Node, error)
	ListChildren(id string) ([]string, error)
}

// -----------------------------------------------------------------------------
// Phase 0 Implementation: In-Memory Go Map
// -----------------------------------------------------------------------------

type MemoryStore struct {
	mu    sync.RWMutex
	nodes map[string]*Node
	roots []string // Top-level nodes (e.g. "vulns")
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		nodes: make(map[string]*Node),
		roots: []string{},
	}
}

// AddNode is our internal "Ingestion" method for Phase 0.
func (s *MemoryStore) AddNode(n *Node) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodes[n.ID] = n

	// Heuristic: If ID has no slashes, it's a root.
	// In a real graph, we'd have explicit parent pointers.
	if len(n.ID) > 0 && n.ID[0] != '/' && len(n.Children) > 0 {
		for _, r := range s.roots {
			if r == n.ID {
				return
			}
		}
		s.roots = append(s.roots, n.ID)
	}
}

// GetNode implements Graph.
func (s *MemoryStore) GetNode(id string) (*Node, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Normalize path: remove leading slash
	if len(id) > 0 && id[0] == '/' {
		id = id[1:]
	}

	n, ok := s.nodes[id]
	if !ok {
		return nil, ErrNotFound
	}
	return n, nil
}

// ListChildren implements Graph.
func (s *MemoryStore) ListChildren(id string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Root case
	if id == "" || id == "/" {
		return s.roots, nil
	}

	// Normalize
	if len(id) > 0 && id[0] == '/' {
		id = id[1:]
	}

	n, ok := s.nodes[id]
	if !ok {
		return nil, ErrNotFound
	}
	return n.Children, nil
}
