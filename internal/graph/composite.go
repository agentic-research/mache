package graph

import (
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// CompositeGraph multiplexes multiple Graph backends under path prefixes.
// Mount "browser" → paths under /browser/ route to that sub-graph.
// Mount "iterm"   → paths under /iterm/ route to that sub-graph.
// Root ListChildren returns the list of mount point names.
type CompositeGraph struct {
	mu     sync.RWMutex
	mounts map[string]Graph // prefix → sub-graph

	// callerDepth guards against infinite recursion in GetCallers/GetCallees
	// when a mounted sub-graph delegates back to this CompositeGraph.
	callerDepth atomic.Int32
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

	prefix, subPath, g := c.resolve(id)
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
	n, err := g.GetNode(subPath)
	if err != nil {
		return nil, err
	}
	return c.reprefixNode(prefix, n), nil
}

// ListChildren implements Graph.
func (c *CompositeGraph) ListChildren(id string) ([]string, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	id = strings.TrimPrefix(id, "/")

	// Root: return mount point names (sorted for deterministic readdir)
	if id == "" {
		names := make([]string, 0, len(c.mounts))
		for prefix := range c.mounts {
			names = append(names, prefix)
		}
		sort.Strings(names)
		return names, nil
	}

	prefix, subPath, g := c.resolve(id)
	if g == nil {
		return nil, ErrNotFound
	}
	var children []string
	var err error
	if subPath == "" {
		children, err = g.ListChildren("")
	} else {
		children, err = g.ListChildren(subPath)
	}
	if err != nil {
		return nil, err
	}
	res := make([]string, len(children))
	for i, child := range children {
		res[i] = prefix + "/" + strings.TrimPrefix(child, "/")
	}
	return res, nil
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

// maxCallerDepth caps recursion when a mounted sub-graph delegates back
// to this CompositeGraph (e.g. a focus router). Without this, GetCallers
// would stack-overflow.
const maxCallerDepth = 2

// GetCallers implements Graph. Searches all mounted sub-graphs.
func (c *CompositeGraph) GetCallers(token string) ([]*Node, error) {
	if c.callerDepth.Add(1) > maxCallerDepth {
		c.callerDepth.Add(-1)
		return nil, nil
	}
	defer c.callerDepth.Add(-1)

	c.mu.RLock()
	defer c.mu.RUnlock()

	var all []*Node
	for prefix, g := range c.mounts {
		nodes, err := g.GetCallers(token)
		if err != nil {
			continue
		}
		for _, n := range nodes {
			all = append(all, c.reprefixNode(prefix, n))
		}
	}
	return all, nil
}

// GetCallees implements Graph.
func (c *CompositeGraph) GetCallees(id string) ([]*Node, error) {
	if c.callerDepth.Add(1) > maxCallerDepth {
		c.callerDepth.Add(-1)
		return nil, nil
	}
	defer c.callerDepth.Add(-1)

	c.mu.RLock()
	defer c.mu.RUnlock()

	prefix, subPath, g := c.resolve(id)
	if g == nil {
		return nil, ErrNotFound
	}
	nodes, err := g.GetCallees(subPath)
	if err != nil {
		return nil, err
	}
	res := make([]*Node, len(nodes))
	for i, n := range nodes {
		res[i] = c.reprefixNode(prefix, n)
	}
	return res, nil
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
	// Re-prefix paths in the result so the caller sees full composite paths
	if result != nil {
		if result.Path != "" && !strings.HasPrefix(result.Path, prefix+"/") {
			result.Path = prefix + "/" + strings.TrimPrefix(result.Path, "/")
		}
		if result.NodeID != "" && !strings.HasPrefix(result.NodeID, prefix+"/") {
			result.NodeID = prefix + "/" + strings.TrimPrefix(result.NodeID, "/")
		}
	}
	return result, nil
}

// reprefixNode returns a shallow copy of n with ID and Children prefixed by the mount point.
func (c *CompositeGraph) reprefixNode(prefix string, n *Node) *Node {
	nCopy := *n
	nCopy.ID = prefix + "/" + nCopy.ID
	if len(nCopy.Children) > 0 {
		nCopy.Children = make([]string, len(n.Children))
		for i, child := range n.Children {
			nCopy.Children[i] = prefix + "/" + child
		}
	}
	return &nCopy
}

// Verify interface compliance at compile time.
var _ Graph = (*CompositeGraph)(nil)
