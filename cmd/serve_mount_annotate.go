package cmd

import (
	"github.com/agentic-research/mache/internal/graph"
)

// scopedItem is the {path, mount} shape MCP handlers emit for results
// that carry a CompositeGraph mount prefix in their node ID. Used by
// find_callers, find_callees, find_definition when running on a
// composite (cross-repo) graph.
type scopedItem struct {
	Path  string `json:"path"`
	Mount string `json:"mount,omitempty"`
}

// annotateMounts returns scoped items if g is a *CompositeGraph and
// at least one path actually carries a mount prefix; nil otherwise.
// Handlers can emit the annotated shape only when annotation adds
// information — single-source serves and composite serves where the
// query never crosses a mount keep the legacy shape.
func annotateMounts(g graph.Graph, paths []string) []scopedItem {
	cg, ok := g.(*graph.CompositeGraph)
	if !ok || len(paths) == 0 {
		return nil
	}
	out := make([]scopedItem, 0, len(paths))
	anyMounted := false
	for _, p := range paths {
		m := cg.MountPrefixOf(p)
		if m != "" {
			anyMounted = true
		}
		out = append(out, scopedItem{Path: p, Mount: m})
	}
	if !anyMounted {
		return nil
	}
	return out
}
