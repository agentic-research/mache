package graph

import (
	"errors"
	"io/fs"
	"sync"
)

var ErrNotFound = errors.New("node not found")

// ContentRef is a recipe for lazily resolving file content from a backing store.
// Instead of storing the full byte content in RAM, we store enough info to re-fetch it on demand.
type ContentRef struct {
	DBPath     string // Path to the SQLite database
	RecordID   string // Row ID in the results table
	Template   string // Content template to re-render
	ContentLen int64  // Pre-computed rendered byte length
}

// SourceOrigin tracks the byte range of a construct in its source file.
// Used by write-back to splice edits into the original source.
type SourceOrigin struct {
	FilePath  string
	StartByte uint32
	EndByte   uint32
}

// Node is the universal primitive.
// The Mode field explicitly declares whether this is a file or directory.
type Node struct {
	ID         string
	Mode       fs.FileMode       // fs.ModeDir for directories, 0 for regular files
	Data       []byte            // Inline content (small files, nil for lazy nodes)
	Ref        *ContentRef       // Lazy content reference (large files, nil for inline nodes)
	Properties map[string][]byte // Metadata / extended attributes
	Children   []string          // Child node IDs (directories only)
	Origin     *SourceOrigin     // Source byte range (nil for dirs, JSON, SQLite nodes)
}

// ContentSize returns the byte length of this node's content,
// regardless of whether it is inline or lazy.
func (n *Node) ContentSize() int64 {
	if n.Data != nil {
		return int64(len(n.Data))
	}
	if n.Ref != nil {
		return n.Ref.ContentLen
	}
	return 0
}

// ContentResolverFunc resolves a ContentRef into byte content.
type ContentResolverFunc func(ref *ContentRef) ([]byte, error)

// Graph is the Read-Only interface for the FUSE layer.
// This allows us to swap the backend later (Memory -> SQLite -> Mmap).
type Graph interface {
	GetNode(id string) (*Node, error)
	ListChildren(id string) ([]string, error)
	ReadContent(id string, buf []byte, offset int64) (int, error)
}

// -----------------------------------------------------------------------------
// Phase 1 Implementation: In-Memory Graph with Lazy Content Resolution
// -----------------------------------------------------------------------------

type MemoryStore struct {
	mu       sync.RWMutex
	nodes    map[string]*Node
	roots    []string // Top-level nodes (e.g. "vulns")
	resolver ContentResolverFunc
	cache    *contentCache
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		nodes: make(map[string]*Node),
		roots: []string{},
	}
}

// SetResolver configures lazy content resolution for nodes with ContentRef.
func (s *MemoryStore) SetResolver(fn ContentResolverFunc) {
	s.resolver = fn
	s.cache = newContentCache(1024)
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

// ReadContent implements Graph. It handles both inline and lazy content.
func (s *MemoryStore) ReadContent(id string, buf []byte, offset int64) (int, error) {
	node, err := s.GetNode(id)
	if err != nil {
		return 0, err
	}

	var data []byte
	if node.Data != nil {
		data = node.Data
	} else if node.Ref != nil {
		data, err = s.resolveContent(id, node.Ref)
		if err != nil {
			return 0, err
		}
	} else {
		return 0, nil
	}

	if offset >= int64(len(data)) {
		return 0, nil
	}
	end := offset + int64(len(buf))
	if end > int64(len(data)) {
		end = int64(len(data))
	}
	n := copy(buf, data[offset:end])
	return n, nil
}

func (s *MemoryStore) resolveContent(id string, ref *ContentRef) ([]byte, error) {
	if s.cache != nil {
		if cached, ok := s.cache.get(id); ok {
			return cached, nil
		}
	}
	if s.resolver == nil {
		return nil, errors.New("no resolver configured for lazy content")
	}
	data, err := s.resolver(ref)
	if err != nil {
		return nil, err
	}
	if s.cache != nil {
		s.cache.put(id, data)
	}
	return data, nil
}

// contentCache is a simple FIFO-evicting bounded cache for resolved content.
type contentCache struct {
	mu      sync.Mutex
	entries map[string][]byte
	keys    []string
	maxSize int
}

func newContentCache(maxSize int) *contentCache {
	return &contentCache{
		entries: make(map[string][]byte, maxSize),
		keys:    make([]string, 0, maxSize),
		maxSize: maxSize,
	}
}

func (c *contentCache) get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.entries[key]
	return v, ok
}

func (c *contentCache) put(key string, value []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[key]; ok {
		c.entries[key] = value
		return
	}
	if len(c.entries) >= c.maxSize {
		evict := c.keys[0]
		c.keys = c.keys[1:]
		delete(c.entries, evict)
	}
	c.entries[key] = value
	c.keys = append(c.keys, key)
}
