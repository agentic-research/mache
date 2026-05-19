package graph

import "sync"

// ContentCache is a FIFO-bounded cache for resolved content bytes.
// Uses RWMutex so concurrent readers (MCP tool calls) don't block each other.
// Replaces the inline sync.Mutex caches that SQLiteGraph and WritableGraph
// previously maintained — the RWMutex promotion is safe because the old code
// never relied on exclusive read access (check-then-set unlocked between steps).
// Shared by MemoryStore, SQLiteGraph, and WritableGraph.
//
// Note: the get→miss→resolve→put sequence in resolveContent is not atomic.
// Under high concurrency, multiple goroutines may miss the cache simultaneously
// and all invoke the resolver for the same key. This is benign — the first
// writer wins via Put's dedup check, and subsequent resolver results are
// discarded. The resolver (SQLite query + template render) is idempotent.
type ContentCache struct {
	mu      sync.RWMutex
	entries map[string][]byte
	keys    []string
	maxSize int
}

// NewContentCache creates a FIFO-bounded content cache.
func NewContentCache(maxSize int) *ContentCache {
	return &ContentCache{
		entries: make(map[string][]byte, maxSize),
		keys:    make([]string, 0, maxSize),
		maxSize: maxSize,
	}
}

// Get retrieves cached content. Safe for concurrent readers.
func (c *ContentCache) Get(key string) ([]byte, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.entries[key]
	return v, ok
}

// Put stores content, evicting the oldest entry if at capacity.
// Deduplicates: if key already exists, updates the value without
// adding a duplicate key entry.
func (c *ContentCache) Put(key string, value []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[key]; ok {
		c.entries[key] = value
		return
	}
	if len(c.entries) >= c.maxSize {
		evict := c.keys[0]
		copy(c.keys, c.keys[1:])
		c.keys = c.keys[:len(c.keys)-1]
		delete(c.entries, evict)
	}
	c.entries[key] = value
	c.keys = append(c.keys, key)
}

// Delete removes a key from both the map and the keys slice.
// Invariant: entries and keys are always in sync — every key in the map
// has exactly one entry in the keys slice, maintained by Put (dedup)
// and Delete (linear scan + remove). The break-after-first-match in the
// scan is correct because Put never creates duplicate key entries.
func (c *ContentCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[key]; !ok {
		return
	}
	delete(c.entries, key)
	for i, k := range c.keys {
		if k == key {
			copy(c.keys[i:], c.keys[i+1:])
			c.keys = c.keys[:len(c.keys)-1]
			break
		}
	}
}
