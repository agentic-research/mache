package graph

import (
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"time"
)

// CompositeGraph multiplexes multiple Graph backends under path prefixes.
// Mount "browser" → paths under /browser/ route to that sub-graph.
// Mount "iterm"   → paths under /iterm/ route to that sub-graph.
// Root ListChildren returns the list of mount point names.
type CompositeGraph struct {
	mu     sync.RWMutex
	mounts map[string]Graph // prefix → sub-graph
}

// NewCompositeGraph creates an empty composite graph.
func NewCompositeGraph() *CompositeGraph {
	return &CompositeGraph{mounts: make(map[string]Graph)}
}

// Mount registers a sub-graph under the given prefix.
// Paths like "/<prefix>/..." are routed to this graph with the prefix stripped.
func (c *CompositeGraph) Mount(prefix string, g Graph) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.mounts[prefix]; ok {
		return fmt.Errorf("mount %q already exists", prefix)
	}
	c.mounts[prefix] = g
	return nil
}

// Unmount removes a previously mounted sub-graph.
func (c *CompositeGraph) Unmount(prefix string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.mounts[prefix]; !ok {
		return fmt.Errorf("mount %q not found", prefix)
	}
	delete(c.mounts, prefix)
	return nil
}

// resolve splits id into (prefix, sub-path, sub-graph).
// Returns ("", "", nil) if no mount matches.
func (c *CompositeGraph) resolve(id string) (string, string, Graph) {
	id = strings.TrimPrefix(id, "/")
	if id == "" {
		return "", "", nil
	}
	prefix, subPath, _ := strings.Cut(id, "/")
	g, ok := c.mounts[prefix]
	if !ok {
		return "", "", nil
	}
	return prefix, subPath, g
}

// GetNode implements Graph.
func (c *CompositeGraph) GetNode(id string) (*Node, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	id = strings.TrimPrefix(id, "/")
	if id == "" {
		return &Node{
			ID:      "",
			Mode:    fs.ModeDir | 0o555,
			ModTime: time.Now(),
		}, nil
	}

	_, subPath, g := c.resolve(id)
	if g == nil {
		return nil, ErrNotFound
	}
	// Mount point directory itself (e.g., "browser" with no sub-path)
	if subPath == "" {
		return &Node{
			ID:      id,
			Mode:    fs.ModeDir | 0o555,
			ModTime: time.Now(),
		}, nil
	}
	return g.GetNode(subPath)
}

// ListChildren implements Graph.
func (c *CompositeGraph) ListChildren(id string) ([]string, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	id = strings.TrimPrefix(id, "/")

	// Root: return mount point names
	if id == "" {
		names := make([]string, 0, len(c.mounts))
		for prefix := range c.mounts {
			names = append(names, prefix)
		}
		return names, nil
	}

	prefix, subPath, g := c.resolve(id)
	if g == nil {
		return nil, ErrNotFound
	}
	_ = prefix
	// Delegate to sub-graph's root or sub-path
	if subPath == "" {
		return g.ListChildren("")
	}
	return g.ListChildren(subPath)
}

// ReadContent implements Graph.
func (c *CompositeGraph) ReadContent(id string, buf []byte, offset int64) (int, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	_, subPath, g := c.resolve(id)
	if g == nil {
		return 0, ErrNotFound
	}
	return g.ReadContent(subPath, buf, offset)
}

// GetCallers implements Graph. Searches all mounted sub-graphs.
func (c *CompositeGraph) GetCallers(token string) ([]*Node, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var all []*Node
	for _, g := range c.mounts {
		nodes, err := g.GetCallers(token)
		if err != nil {
			continue
		}
		all = append(all, nodes...)
	}
	return all, nil
}

// GetCallees implements Graph.
func (c *CompositeGraph) GetCallees(id string) ([]*Node, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	_, subPath, g := c.resolve(id)
	if g == nil {
		return nil, ErrNotFound
	}
	return g.GetCallees(subPath)
}

// Invalidate implements Graph.
func (c *CompositeGraph) Invalidate(id string) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	_, subPath, g := c.resolve(id)
	if g != nil {
		g.Invalidate(subPath)
	}
}

// Act implements Graph. Routes to the appropriate sub-graph.
func (c *CompositeGraph) Act(id, action, payload string) (*ActionResult, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	prefix, subPath, g := c.resolve(id)
	if g == nil {
		return nil, ErrNotFound
	}
	result, err := g.Act(subPath, action, payload)
	if err != nil {
		return nil, err
	}
	// Re-prefix the path in the result so the caller sees full paths
	if result != nil && result.Path != "" && !strings.HasPrefix(result.Path, prefix) {
		result.Path = prefix + "/" + result.Path
	}
	return result, nil
}

// Verify interface compliance at compile time.
var _ Graph = (*CompositeGraph)(nil)
