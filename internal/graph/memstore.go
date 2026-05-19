package graph

import (
	"database/sql"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RoaringBitmap/roaring"
)

// -----------------------------------------------------------------------------
// Phase 1 Implementation: In-Memory Graph with Lazy Content Resolution
// -----------------------------------------------------------------------------

type MemoryStore struct {
	mu       sync.RWMutex
	nodes    map[string]*Node
	roots    []string            // Top-level nodes (e.g. "vulns")
	rootsSet map[string]struct{} // O(1) dedup for AddRoot
	resolver ContentResolverFunc
	cache    *ContentCache
	refs     map[string][]string // token -> []nodeID (callers: who calls token)
	defs     map[string][]string // token -> []construct_dir_id (definitions: where token is defined)

	// Memoized deep-copy snapshots of refs and defs.
	//
	// Without this, every DefsMap/RefsMap call re-allocates the
	// whole map + a new slice per entry. find_smells e2e profiling
	// (mache-6b6da6 phase 3) showed `MemoryStore.DefsMap` at 52% of
	// `get_impact`'s heap delta and 32% of `get_overview`'s — the
	// copies were dominating these tools' allocation budgets even
	// on a 4-package toy fixture.
	//
	// Snapshots are populated lazily on first access after each
	// invalidation (under RLock) and stored atomically. AddDef /
	// AddRef / DeleteFileNodes invalidate by storing nil before
	// releasing their write lock; the next reader rebuilds. The
	// returned map is shared across concurrent readers — callers
	// must NOT mutate (every existing caller is read-only;
	// LookupDef is the right API for callers who want to mutate
	// the result).
	defsSnap atomic.Pointer[map[string][]string]
	refsSnap atomic.Pointer[map[string][]string]

	// Roaring bitmap index: file path → set of node internal IDs.
	// Enables O(k) DeleteFileNodes and ShiftOrigins instead of O(N) full scan.
	fileToNodes map[string]*roaring.Bitmap // FilePath → bitmap of internal node IDs
	nodeIntID   map[string]uint32          // Node.ID → internal bitmap uint32 ID
	intToNodeID []string                   // reverse: uint32 → Node.ID
	nextIntID   uint32                     // monotonic counter

	// Diagnostics: last write status per node path (for _diagnostics/ virtual dir).
	WriteStatus sync.Map // node path (string) → error message (string)

	// Temp-file SQLite sidecar for cross-reference queries.
	// Same schema as SQLiteGraph's .refs.db (node_refs + file_ids + mache_refs vtab).
	// Uses a temp file (not :memory:) because the vtab's xFilter needs a second
	// pool connection that can see the same tables — :memory: isolates per-connection.
	refsDB     *sql.DB
	refsDBPath string // temp file path, cleaned up on Close
	dbID       string // unique ID for vtab registry
	flushOnce  sync.Once
	flushErr   error

	extractor CallExtractor

	// Live graph: file mtime tracking and on-demand refresh.
	fileMtimes map[string]time.Time        // source file → mtime at index time
	refresher  func(filePath string) error // called when a source file is stale
	refreshMu  sync.Map                    // filePath → *sync.Mutex (per-file refresh serialization)
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		nodes:       make(map[string]*Node),
		roots:       []string{},
		rootsSet:    make(map[string]struct{}),
		refs:        make(map[string][]string),
		defs:        make(map[string][]string),
		fileToNodes: make(map[string]*roaring.Bitmap),
		nodeIntID:   make(map[string]uint32),
		fileMtimes:  make(map[string]time.Time),
	}
}

// SetCallExtractor configures the parser for on-demand callee resolution.
func (s *MemoryStore) SetCallExtractor(fn CallExtractor) {
	s.extractor = fn
}

// SetResolver configures lazy content resolution for nodes with ContentRef.
// Cache size scales with node count: 25% of nodes, floor 1024, ceiling 16384.
func (s *MemoryStore) SetResolver(fn ContentResolverFunc) {
	s.resolver = fn
	size := min(max(len(s.nodes)/4, 1024), 16384)
	s.cache = NewContentCache(size)
}

// RootIDs returns a copy of the top-level root node IDs.
func (s *MemoryStore) RootIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, len(s.roots))
	copy(ids, s.roots)
	return ids
}

// ListChildStats returns stat snapshots for all children under a single RLock.
// Returns []NodeStat (value types, no aliasing) — safe to read without a lock.
// Eliminates N individual GetNode calls during FUSE/NFS readdir.
// Missing children are silently skipped.
func (s *MemoryStore) ListChildStats(id string) ([]NodeStat, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var childIDs []string
	if id == "" || id == "/" {
		childIDs = s.roots
	} else {
		id = NormalizeID(id)
		n, ok := s.nodes[id]
		if !ok {
			return nil, ErrNotFound
		}
		childIDs = n.Children
	}

	stats := make([]NodeStat, 0, len(childIDs))
	for _, cid := range childIDs {
		if n, ok := s.nodes[cid]; ok {
			stats = append(stats, NodeStat{
				ID:          n.ID,
				IsDir:       n.Mode.IsDir(),
				ContentSize: n.ContentSize(),
				ModTime:     n.ModTime,
				HasOrigin:   n.Origin != nil,
			})
		}
	}
	return stats, nil
}

// GetCallers implements Graph.
func (s *MemoryStore) GetCallers(token string) ([]*Node, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ids, ok := s.refs[token]
	if !ok {
		return nil, nil
	}

	var nodes []*Node
	for _, id := range ids {
		if n, ok := s.nodes[id]; ok {
			nodes = append(nodes, n)
		}
	}
	return nodes, nil
}

// Invalidate is a no-op for MemoryStore — nodes are updated in-place.
func (s *MemoryStore) Invalidate(id string) {}

// Act returns ErrActNotSupported — MemoryStore is a passive code graph.
func (s *MemoryStore) Act(id, action, payload string) (*ActionResult, error) {
	return nil, ErrActNotSupported
}

// GetNode implements Graph.
func (s *MemoryStore) GetNode(id string) (*Node, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	id = NormalizeID(id)
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

	if id == "" || id == "/" {
		out := make([]string, len(s.roots))
		copy(out, s.roots)
		return out, nil
	}

	id = NormalizeID(id)
	n, ok := s.nodes[id]
	if !ok {
		return nil, ErrNotFound
	}
	out := make([]string, len(n.Children))
	copy(out, n.Children)
	return out, nil
}

// ReadContent implements Graph. It handles both inline and lazy content.
// If the node has a SourceOrigin and the source file's mtime has changed,
// the refresher callback is invoked to re-ingest the file before reading.
func (s *MemoryStore) ReadContent(id string, buf []byte, offset int64) (int, error) {
	node, err := s.GetNode(id)
	if err != nil {
		return 0, err
	}

	// Live graph: check staleness for nodes with source origins
	if node.Origin != nil && node.Origin.FilePath != "" && s.refresher != nil {
		if s.IsFileStale(node.Origin.FilePath) {
			filePath := node.Origin.FilePath
			// Per-file mutex allows parallel refreshes of different files
			muI, _ := s.refreshMu.LoadOrStore(filePath, &sync.Mutex{})
			fileMu := muI.(*sync.Mutex)
			fileMu.Lock()
			// Double-check after acquiring lock (another goroutine may have refreshed)
			if s.IsFileStale(filePath) {
				if err := s.refresher(filePath); err != nil {
					log.Printf("live graph: refresh failed for %s: %v", filePath, err)
				}
			}
			fileMu.Unlock()
			// Re-fetch node after refresh (content may have changed)
			node, err = s.GetNode(id)
			if err != nil {
				return 0, err
			}
		}
	}

	var data []byte
	if node.DraftData != nil {
		data = node.DraftData
	} else if node.Data != nil {
		data = node.Data
	} else if node.Ref != nil {
		data, err = s.resolveContent(id, node.Ref)
		if err != nil {
			return 0, err
		}
	} else {
		return 0, nil
	}

	return SliceContent(data, buf, offset), nil
}

func (s *MemoryStore) resolveContent(id string, ref *ContentRef) ([]byte, error) {
	if s.cache != nil {
		if cached, ok := s.cache.Get(id); ok {
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
		s.cache.Put(id, data)
	}
	return data, nil
}
