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

// mountPrefixer is satisfied by anything that can resolve a node ID
// to its mount prefix. *graph.CompositeGraph implements this directly;
// *lazyGraph implements it by delegating to its inner graph if that
// inner is itself a CompositeGraph. The interface lets annotateMounts
// reach the composite even when handlers receive a wrapped graph.
type mountPrefixer interface {
	MountPrefixOf(id string) string
}

// annotateMounts returns scoped items if g exposes a mount-prefix
// resolver and at least one path actually carries a mount prefix;
// nil otherwise. Handlers emit the annotated shape only when
// annotation adds information — single-source serves and composite
// serves where the query never crosses a mount keep the legacy shape.
//
// Used to fail in production: g passed to MCP handlers is a *lazyGraph
// (the registry wrapper), not the underlying *CompositeGraph, so the
// type assertion missed and every cross-mount call emitted the legacy
// single-string shape. Tests bypassed lazyGraph and looked correct;
// production never hit the annotated branch. Switching to an interface
// lets lazyGraph forward MountPrefixOf to its inner.
func annotateMounts(g graph.Graph, paths []string) []scopedItem {
	mp, ok := g.(mountPrefixer)
	if !ok || len(paths) == 0 {
		return nil
	}
	out := make([]scopedItem, 0, len(paths))
	anyMounted := false
	for _, p := range paths {
		m := mp.MountPrefixOf(p)
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
