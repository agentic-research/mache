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

	// mountTime is captured at construction and used as the ModTime for the
	// synthetic root and mount-point directory nodes. Stable across calls so
	// NFS/FUSE attribute caches don't invalidate on every readdir.
	mountTime time.Time

	// callerDepth guards against infinite recursion in GetCallers/GetCallees
	// when a mounted sub-graph delegates back to this CompositeGraph.
	callerDepth atomic.Int32

	// extractor enables cross-mount callees resolution. When set, GetCallees
	// extracts calls from the routed source itself (in addition to running
	// the sub-graph's local resolution) and looks each one up against the
	// federated DefsMap. Tokens that resolve to a different mount produce
	// extra results in the merged response. nil means cross-mount callees
	// are off — sub-graphs still run their own GetCallees as before.
	extractor CallExtractor
}

// NewCompositeGraph creates an empty composite graph.
func NewCompositeGraph() *CompositeGraph {
	return &CompositeGraph{
		mounts:    make(map[string]Graph),
		mountTime: time.Now(),
	}
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

// MountPrefixOf returns the mount-name prefix that would route id to
// a mounted sub-graph, or "" if id doesn't resolve to any mount (the
// virtual root or an unknown prefix).
//
// MCP handlers use this to annotate cross-repo results with their
// mount of origin without having to parse node IDs themselves.
func (c *CompositeGraph) MountPrefixOf(id string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	prefix, _, _ := c.resolve(id)
	return prefix
}

// defsMapper is the optional interface a sub-graph implements when
// it has a token → dir IDs definition index. Both MemoryStore and
// SQLiteGraph implement this; CompositeGraph aggregates over them.
type defsMapper interface {
	DefsMap() map[string][]string
}

// DefsMap aggregates token → dir IDs across every mount that
// implements DefsMap(), prefixing each dir ID with its mount name.
// Sub-graphs that don't expose a defs index are skipped.
//
// Lets find_definition and search work correctly on a composite
// graph: a token defined in mount A and mount B returns
// ["A/path/to/def", "B/path/to/def"] — agents see all definitions
// across mounts in one query.
func (c *CompositeGraph) DefsMap() map[string][]string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string][]string)
	for prefix, g := range c.mounts {
		dp, ok := g.(defsMapper)
		if !ok {
			continue
		}
		for token, ids := range dp.DefsMap() {
			for _, id := range ids {
				var wrapped string
				if id == "" {
					wrapped = prefix
				} else {
					wrapped = prefix + "/" + id
				}
				out[token] = append(out[token], wrapped)
			}
		}
	}
	return out
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
			ModTime: c.mountTime,
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
			ModTime: c.mountTime,
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

// ListChildStats implements Graph.
func (c *CompositeGraph) ListChildStats(id string) ([]NodeStat, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	id = strings.TrimPrefix(id, "/")

	// Root: return mount point names as directory stats (sorted for deterministic readdir)
	if id == "" {
		names := make([]string, 0, len(c.mounts))
		for prefix := range c.mounts {
			names = append(names, prefix)
		}
		sort.Strings(names)
		stats := make([]NodeStat, len(names))
		for i, name := range names {
			stats[i] = NodeStat{
				ID:      name,
				IsDir:   true,
				ModTime: c.mountTime,
			}
		}
		return stats, nil
	}

	prefix, subPath, g := c.resolve(id)
	if g == nil {
		return nil, ErrNotFound
	}
	var subStats []NodeStat
	var err error
	if subPath == "" {
		subStats, err = g.ListChildStats("")
	} else {
		subStats, err = g.ListChildStats(subPath)
	}
	if err != nil {
		return nil, err
	}
	res := make([]NodeStat, len(subStats))
	for i, s := range subStats {
		res[i] = s
		res[i].ID = prefix + "/" + strings.TrimPrefix(s.ID, "/")
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

// GetCallees implements Graph. Routes to the local mount for in-mount
// resolution; if a cross-mount CallExtractor is configured (via
// SetCallExtractor), additionally resolves each extracted call against
// the federated DefsMap so callees living in OTHER mounts surface in
// the response. Local results win on dedupe; cross-mount results
// supplement.
func (c *CompositeGraph) GetCallees(id string) ([]*Node, error) {
	if c.callerDepth.Add(1) > maxCallerDepth {
		c.callerDepth.Add(-1)
		return nil, nil
	}
	defer c.callerDepth.Add(-1)

	c.mu.RLock()
	prefix, subPath, g := c.resolve(id)
	extractor := c.extractor
	c.mu.RUnlock()

	if g == nil {
		return nil, ErrNotFound
	}

	// Phase 1: route to local mount and re-prefix the results.
	nodes, err := g.GetCallees(subPath)
	if err != nil {
		return nil, err
	}
	res := make([]*Node, 0, len(nodes))
	seen := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		wrapped := c.reprefixNode(prefix, n)
		res = append(res, wrapped)
		seen[wrapped.ID] = true
	}

	// Phase 2: cross-mount resolution. Skip if no extractor was wired
	// or if the local mount didn't expose the construct shape we need
	// (no source child / no language hint).
	if extractor != nil {
		extra := c.crossMountCallees(g, id, subPath, extractor)
		for _, n := range extra {
			if seen[n.ID] {
				continue
			}
			res = append(res, n)
			seen[n.ID] = true
		}
	}
	return res, nil
}

// crossMountCallees re-extracts calls from id's source content using
// the composite's own extractor and resolves each against the
// federated DefsMap. Returns nodes whose IDs are composite-namespaced
// (mount/path/...). Errors are swallowed — cross-mount resolution is
// a best-effort supplement, not a replacement for local resolution.
func (c *CompositeGraph) crossMountCallees(local Graph, fullID, subPath string, extractor CallExtractor) []*Node {
	// Find the construct node and its source child.
	node, err := local.GetNode(subPath)
	if err != nil || node == nil || !node.Mode.IsDir() {
		return nil
	}
	var sourceID string
	for _, childID := range node.Children {
		base := childID
		if i := strings.LastIndex(childID, "/"); i >= 0 {
			base = childID[i+1:]
		}
		if base == "source" {
			sourceID = childID
			break
		}
	}
	if sourceID == "" {
		return nil
	}

	var langName string
	if v, ok := node.Properties["lang"]; ok {
		langName = string(v)
	}
	if langName == "" {
		return nil
	}

	srcNode, err := local.GetNode(sourceID)
	if err != nil || srcNode == nil {
		return nil
	}
	buf := make([]byte, srcNode.ContentSize())
	if _, err := local.ReadContent(sourceID, buf, 0); err != nil {
		return nil
	}

	qcalls, err := extractor(buf, sourceID, langName)
	if err != nil {
		return nil
	}

	defs := c.DefsMap()
	var results []*Node
	seen := make(map[string]bool)
	for _, qc := range qcalls {
		// Resolution order matches MemoryStore: qualified first, then
		// bare. No import-path fallback here — that's MemoryStore's
		// per-language refinement, not the cross-mount contract.
		keys := make([]string, 0, 2)
		if qc.Qualifier != "" {
			keys = append(keys, qc.Qualifier+"."+qc.Token)
		}
		keys = append(keys, qc.Token)

		for _, key := range keys {
			defIDs, ok := defs[key]
			if !ok {
				continue
			}
			for _, defID := range defIDs {
				if defID == fullID || seen[defID] {
					continue
				}
				results = append(results, &Node{ID: defID})
				seen[defID] = true
			}
			break
		}
	}
	return results
}

// SetCallExtractor wires a tree-sitter call extractor onto the
// composite for cross-mount callees resolution. With nil or no
// extractor set, GetCallees only consults the local mount's defs
// index — sub-graphs that already have their own extractor still
// resolve in-mount callees as before.
func (c *CompositeGraph) SetCallExtractor(fn CallExtractor) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.extractor = fn
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
