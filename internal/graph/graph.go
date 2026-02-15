package graph

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/RoaringBitmap/roaring"
	"github.com/agentic-research/mache/internal/refsvtab"
	_ "modernc.org/sqlite"
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
	ModTime    time.Time         // Modification time
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

// Graph is the interface for the FUSE layer.
// This allows us to swap the backend later (Memory -> SQLite -> Mmap).
type Graph interface {
	GetNode(id string) (*Node, error)
	ListChildren(id string) ([]string, error)
	ReadContent(id string, buf []byte, offset int64) (int, error)
	GetCallers(token string) ([]*Node, error)
	// Invalidate evicts cached data for a node (size, content).
	// Called after write-back to force re-render on next access.
	Invalidate(id string)
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
	refs     map[string][]string // token -> []nodeID

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
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		nodes:       make(map[string]*Node),
		roots:       []string{},
		refs:        make(map[string][]string),
		fileToNodes: make(map[string]*roaring.Bitmap),
		nodeIntID:   make(map[string]uint32),
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
	s.indexNode(n)
}

// indexNode assigns an internal bitmap ID and registers the node in fileToNodes.
// Must be called with s.mu held.
func (s *MemoryStore) indexNode(n *Node) {
	if n.Origin == nil {
		return
	}
	// Assign internal ID if not already assigned
	intID, ok := s.nodeIntID[n.ID]
	if !ok {
		intID = s.nextIntID
		s.nextIntID++
		s.nodeIntID[n.ID] = intID
		// Grow reverse map
		for uint32(len(s.intToNodeID)) <= intID {
			s.intToNodeID = append(s.intToNodeID, "")
		}
		s.intToNodeID[intID] = n.ID
	}
	// Set bit in file→nodes bitmap
	bm, exists := s.fileToNodes[n.Origin.FilePath]
	if !exists {
		bm = roaring.New()
		s.fileToNodes[n.Origin.FilePath] = bm
	}
	bm.Add(intID)
}

// AddRef records a reference from a file (nodeID) to a token.
func (s *MemoryStore) AddRef(token, nodeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refs[token] = append(s.refs[token], nodeID)
	return nil
}

// DeleteFileNodes removes all nodes that originated from the given source file.
// Uses the roaring bitmap index for O(k) lookup instead of O(N) full scan.
func (s *MemoryStore) DeleteFileNodes(filePath string) {
	// Canonicalize path to match Ingest behavior
	if realPath, err := filepath.EvalSymlinks(filePath); err == nil {
		filePath = realPath
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// 1. Collect IDs to delete via bitmap index
	bm, hasBitmap := s.fileToNodes[filePath]
	var toDelete []string
	if hasBitmap {
		it := bm.Iterator()
		for it.HasNext() {
			intID := it.Next()
			if int(intID) < len(s.intToNodeID) {
				nodeID := s.intToNodeID[intID]
				if nodeID != "" {
					toDelete = append(toDelete, nodeID)
				}
			}
		}
	} else {
		// Fallback: full scan for nodes not yet indexed (e.g. added before indexing)
		for id, n := range s.nodes {
			if n.Origin != nil && n.Origin.FilePath == filePath {
				toDelete = append(toDelete, id)
			}
		}
	}

	// 2. Build deletion set for O(1) lookups
	deleteSet := make(map[string]struct{}, len(toDelete))
	for _, id := range toDelete {
		deleteSet[id] = struct{}{}
		delete(s.nodes, id)
		// Clean up bitmap index entries
		if intID, ok := s.nodeIntID[id]; ok {
			if hasBitmap {
				bm.Remove(intID)
			}
			delete(s.nodeIntID, id)
			if int(intID) < len(s.intToNodeID) {
				s.intToNodeID[intID] = ""
			}
		}
	}

	// Remove empty bitmap
	if hasBitmap && bm.IsEmpty() {
		delete(s.fileToNodes, filePath)
	}

	// 3. Clean up children pointers in remaining nodes
	for _, n := range s.nodes {
		if n.Mode.IsDir() && len(n.Children) > 0 {
			newChildren := n.Children[:0]
			changed := false
			for _, c := range n.Children {
				if _, del := deleteSet[c]; del {
					changed = true
				} else {
					newChildren = append(newChildren, c)
				}
			}
			if changed {
				n.Children = newChildren
			}
		}
	}
}

// ShiftOrigins adjusts StartByte/EndByte for all nodes from filePath whose
// origin starts at or after afterByte. delta is the signed byte count change
// (positive = content grew, negative = content shrank).
// Called after splice, BEFORE re-ingest, to keep sibling offsets correct.
func (s *MemoryStore) ShiftOrigins(filePath string, afterByte uint32, delta int32) {
	s.mu.Lock()
	defer s.mu.Unlock()

	bm, ok := s.fileToNodes[filePath]
	if !ok {
		return
	}

	it := bm.Iterator()
	for it.HasNext() {
		intID := it.Next()
		if int(intID) >= len(s.intToNodeID) {
			continue
		}
		nodeID := s.intToNodeID[intID]
		if nodeID == "" {
			continue
		}
		n, exists := s.nodes[nodeID]
		if !exists || n.Origin == nil {
			continue
		}
		if n.Origin.FilePath != filePath {
			continue
		}
		// Only shift nodes that start at or after the splice point
		if n.Origin.StartByte >= afterByte {
			n.Origin.StartByte = uint32(int32(n.Origin.StartByte) + delta)
			n.Origin.EndByte = uint32(int32(n.Origin.EndByte) + delta)
		}
	}
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

// ---------------------------------------------------------------------------
// MemoryStore SQL query support (in-memory SQLite sidecar)
// ---------------------------------------------------------------------------

// InitRefsDB opens an in-memory SQLite database with the same schema as
// SQLiteGraph's sidecar (node_refs + file_ids + mache_refs vtab).
// Must be called before FlushRefs. Safe to call multiple times (idempotent).
func (s *MemoryStore) InitRefsDB() error {
	if s.refsDB != nil {
		return nil
	}

	refsMod, err := refsvtab.Register()
	if err != nil {
		return err
	}

	// Use a temp file (not :memory:) because the vtab's xFilter runs inside
	// the SQLite engine on the outer connection and needs a SECOND pool
	// connection to query node_refs/file_ids. With :memory:, each connection
	// gets its own isolated database. A temp file + WAL mode lets both
	// connections see the same tables — same pattern as SQLiteGraph's .refs.db.
	tmpFile, err := os.CreateTemp("", "mache-refs-*.db")
	if err != nil {
		return fmt.Errorf("create temp refs db: %w", err)
	}
	refsPath := tmpFile.Name()
	_ = tmpFile.Close()

	db, err := sql.Open("sqlite", refsPath)
	if err != nil {
		_ = os.Remove(refsPath) // cleanup temp file
		return fmt.Errorf("open refs db: %w", err)
	}
	// Allow 2 connections: one for normal queries, one for vtab Filter callbacks.
	// WAL mode ensures concurrent readers don't conflict.
	db.SetMaxOpenConns(2)

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		_ = db.Close()          // ignore close error
		_ = os.Remove(refsPath) // cleanup temp file
		return fmt.Errorf("set WAL mode on refs db: %w", err)
	}

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS node_refs (
			token TEXT PRIMARY KEY,
			bitmap BLOB
		);
		CREATE TABLE IF NOT EXISTS file_ids (
			id INTEGER PRIMARY KEY,
			path TEXT UNIQUE NOT NULL
		);
	`)
	if err != nil {
		_ = db.Close()          // ignore close error
		_ = os.Remove(refsPath) // cleanup temp file
		return fmt.Errorf("create refs tables: %w", err)
	}

	// Generate a unique ID for this DB connection to register with the vtab module.
	// This allows multiple MemoryStore instances (e.g. tests) to coexist without
	// race conditions on a single global refsDB pointer.
	dbID := fmt.Sprintf("mem_%d", time.Now().UnixNano())
	refsMod.RegisterDB(dbID, db)

	// Declare vtab with the unique ID as an argument.
	// The Create method in refs_module.go will look up the DB using this ID.
	query := fmt.Sprintf("CREATE VIRTUAL TABLE IF NOT EXISTS mache_refs USING mache_refs(%s)", dbID)
	if _, err := db.Exec(query); err != nil {
		refsMod.UnregisterDB(dbID)
		_ = db.Close()          // ignore close error
		_ = os.Remove(refsPath) // cleanup temp file
		return fmt.Errorf("create mache_refs vtab: %w", err)
	}

	s.refsDB = db
	s.refsDBPath = refsPath
	s.dbID = dbID
	return nil
}

// FlushRefs writes all accumulated refs (from AddRef) into the in-memory
// SQLite sidecar as roaring bitmaps. Guarded by sync.Once — safe to call
// multiple times; only the first call performs the flush.
func (s *MemoryStore) FlushRefs() error {
	s.flushOnce.Do(func() {
		s.flushErr = s.flushRefsInternal()
	})
	return s.flushErr
}

func (s *MemoryStore) flushRefsInternal() error {
	if s.refsDB == nil {
		return fmt.Errorf("refsDB not initialized: call InitRefsDB first")
	}

	s.mu.RLock()
	refs := s.refs
	s.mu.RUnlock()

	if len(refs) == 0 {
		return nil
	}

	// Build file ID map from all unique paths
	fileIDMap := make(map[string]uint32)
	var nextID uint32
	for _, paths := range refs {
		for _, p := range paths {
			if _, ok := fileIDMap[p]; !ok {
				fileIDMap[p] = nextID
				nextID++
			}
		}
	}

	// Build roaring bitmaps per token
	bitmaps := make(map[string]*roaring.Bitmap, len(refs))
	for token, paths := range refs {
		bm := roaring.New()
		for _, p := range paths {
			bm.Add(fileIDMap[p])
		}
		bitmaps[token] = bm
	}

	// Write both tables in a single transaction
	tx, err := s.refsDB.Begin()
	if err != nil {
		return fmt.Errorf("begin refs flush: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // safe to ignore (no-op if committed)

	fileStmt, err := tx.Prepare("INSERT OR IGNORE INTO file_ids (id, path) VALUES (?, ?)")
	if err != nil {
		return fmt.Errorf("prepare file_ids insert: %w", err)
	}
	defer func() { _ = fileStmt.Close() }() // safe to ignore

	for path, id := range fileIDMap {
		if _, err := fileStmt.Exec(id, path); err != nil {
			return fmt.Errorf("insert file_id %s: %w", path, err)
		}
	}

	refStmt, err := tx.Prepare("INSERT OR REPLACE INTO node_refs (token, bitmap) VALUES (?, ?)")
	if err != nil {
		return fmt.Errorf("prepare node_refs insert: %w", err)
	}
	defer func() { _ = refStmt.Close() }() // safe to ignore

	var buf bytes.Buffer
	for token, bm := range bitmaps {
		buf.Reset()
		if _, err := bm.WriteTo(&buf); err != nil {
			return fmt.Errorf("serialize bitmap for %s: %w", token, err)
		}
		if _, err := refStmt.Exec(token, buf.Bytes()); err != nil {
			return fmt.Errorf("insert ref %s: %w", token, err)
		}
	}

	return tx.Commit()
}

// QueryRefs executes a SQL query against the in-memory refs database,
// which includes the mache_refs virtual table.
func (s *MemoryStore) QueryRefs(query string, args ...any) (*sql.Rows, error) {
	if s.refsDB == nil {
		return nil, fmt.Errorf("refsDB not initialized: call InitRefsDB first")
	}
	return s.refsDB.Query(query, args...)
}

// Close closes the refs database and removes the temp file.
func (s *MemoryStore) Close() error {
	if s.refsDB != nil {
		// Unregister from vtab module to prevent leaks/races
		if mod, err := refsvtab.Register(); err == nil && mod != nil {
			mod.UnregisterDB(s.dbID)
		}

		err := s.refsDB.Close()
		if s.refsDBPath != "" {
			_ = os.Remove(s.refsDBPath) // best-effort cleanup
			_ = os.Remove(s.refsDBPath + "-wal")
			_ = os.Remove(s.refsDBPath + "-shm")
		}
		return err
	}
	return nil
}
