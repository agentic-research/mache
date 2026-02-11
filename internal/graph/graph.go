package graph

import (
	"errors"
	"io/fs"
	"sync"
)

var ErrNotFound = errors.New("node not found")

// Node is the universal primitive.
// The Mode field explicitly declares whether this is a file or directory.
type Node struct {
	ID         string
	Mode       fs.FileMode       // fs.ModeDir for directories, 0 for regular files
	Data       []byte            // Content (files only)
	Properties map[string][]byte // Metadata / extended attributes
	Children   []string          // Child node IDs (directories only)
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

// AddRoot registers a node as a top-level root and adds it to the store.
// Callers must explicitly declare roots — there is no heuristic.
func (s *MemoryStore) AddRoot(n *Node) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodes[n.ID] = n
	for _, r := range s.roots {
		if r == n.ID {
			return
		}
	}
	s.roots = append(s.roots, n.ID)
}

// AddNode adds a non-root node to the store.
func (s *MemoryStore) AddNode(n *Node) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodes[n.ID] = n
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
