package graph

import (
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// GraphCache is a thread-safe MemoryStore with automatic SQLite write-through
// persistence. It provides tree manipulation helpers for building and querying
// hierarchical node structures.
//
// Mutations acquire the write lock, modify the underlying MemoryStore, then
// call ExportSQLite to flush the entire graph to disk. On construction,
// ImportSQLite loads any previously persisted state.
//
// Domain-specific logic (fingerprint matching, eviction policies, custom node
// layouts) is NOT part of GraphCache. Consumers build those on top using the
// tree manipulation primitives and the Store() escape hatch.
type GraphCache struct {
	mu     sync.RWMutex
	store  *MemoryStore
	dbPath string // empty = pure in-memory (no persistence)
}

// NewGraphCache creates a GraphCache backed by SQLite at dbPath.
// If dbPath is empty, the cache is purely in-memory (suitable for tests).
// If dbPath points to an existing SQLite file, the previously persisted
// graph is loaded via ImportSQLite.
func NewGraphCache(dbPath string) *GraphCache {
	c := &GraphCache{
		store:  NewMemoryStore(),
		dbPath: dbPath,
	}
	if dbPath == "" {
		return c
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		log.Printf("graphcache: mkdir %s: %v (in-memory only)", filepath.Dir(dbPath), err)
		c.dbPath = ""
		return c
	}
	if _, err := os.Stat(dbPath); err == nil {
		imported, err := ImportSQLite(dbPath)
		if err != nil {
			log.Printf("graphcache: import %s: %v (starting fresh)", dbPath, err)
		} else {
			c.store = imported
		}
	}
	return c
}

// Store returns the underlying MemoryStore for direct access.
// Use this for batch operations: mutate the store directly, then call Persist()
// once. Callers are responsible for coordinating with GraphCache.mu if mixing
// direct store access with GraphCache mutation methods.
func (c *GraphCache) Store() *MemoryStore {
	return c.store
}

// persistLocked flushes the graph to SQLite. Must be called with c.mu held.
func (c *GraphCache) persistLocked() {
	if c.dbPath == "" {
		return
	}
	if err := ExportSQLite(c.store, c.dbPath); err != nil {
		log.Printf("graphcache: export %s: %v", c.dbPath, err)
	}
}

// --- Mutation methods (all write-through) ---

// PutDir ensures a directory node exists at the given id.
// If isRoot is true and the node is new, it is registered as a root node.
// Returns true if a new node was created.
func (c *GraphCache) PutDir(id string, isRoot bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, err := c.store.GetNode(id); err == nil {
		return false
	}
	node := &Node{
		ID:       id,
		Mode:     fs.ModeDir,
		ModTime:  time.Now(),
		Children: []string{},
	}
	if isRoot {
		c.store.AddRoot(node)
	} else {
		c.store.AddNode(node)
	}
	c.persistLocked()
	return true
}

// PutFile creates or overwrites a file node with the given data.
// The node's ModTime is set to time.Now().
func (c *GraphCache) PutFile(id string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.store.AddNode(&Node{
		ID:      id,
		Data:    data,
		ModTime: time.Now(),
	})
	c.persistLocked()
}

// AppendChild adds childID to the Children list of parentID if not already
// present. Returns false if the parent does not exist.
func (c *GraphCache) AppendChild(parentID, childID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	parent, err := c.store.GetNode(parentID)
	if err != nil {
		return false
	}
	if slices.Contains(parent.Children, childID) {
		return true // already present
	}
	parent.Children = append(parent.Children, childID)
	c.persistLocked()
	return true
}

// RemoveChild removes childID from the Children list of parentID.
// Returns false if the parent does not exist.
func (c *GraphCache) RemoveChild(parentID, childID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	parent, err := c.store.GetNode(parentID)
	if err != nil {
		return false
	}
	filtered := make([]string, 0, len(parent.Children))
	for _, ch := range parent.Children {
		if ch != childID {
			filtered = append(filtered, ch)
		}
	}
	parent.Children = filtered
	c.persistLocked()
	return true
}

// ClearChildren sets the Children list of a directory node to empty.
// Returns false if the node does not exist.
// Combined with RemoveChild on the parent, this effectively "deletes" a subtree:
// ExportSQLite only walks from roots, so orphaned nodes are not persisted.
func (c *GraphCache) ClearChildren(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	node, err := c.store.GetNode(id)
	if err != nil {
		return false
	}
	node.Children = []string{}
	c.persistLocked()
	return true
}

// --- Read methods ---

// GetNode returns the node at the given id, or (nil, false) if not found.
func (c *GraphCache) GetNode(id string) (*Node, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	node, err := c.store.GetNode(id)
	if err != nil {
		return nil, false
	}
	return node, true
}

// GetData returns the Data field of a file node, or (nil, false) if the node
// does not exist or has no data.
func (c *GraphCache) GetData(id string) ([]byte, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	node, err := c.store.GetNode(id)
	if err != nil || len(node.Data) == 0 {
		return nil, false
	}
	return node.Data, true
}

// ListChildren returns the Children of a directory node, or (nil, false) if the
// node does not exist or is not a directory.
func (c *GraphCache) ListChildren(id string) ([]string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	node, err := c.store.GetNode(id)
	if err != nil || !node.Mode.IsDir() {
		return nil, false
	}
	return node.Children, true
}

// RootIDs returns all root node IDs.
func (c *GraphCache) RootIDs() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.store.RootIDs()
}

// --- Persistence ---

// Persist flushes the entire graph to SQLite. Called automatically by mutation
// methods. Use explicitly after batch operations via Store().
func (c *GraphCache) Persist() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.dbPath == "" {
		return nil
	}
	return ExportSQLite(c.store, c.dbPath)
}

// Close is a no-op. ExportSQLite manages its own DB connections per call.
func (c *GraphCache) Close() error {
	return nil
}
