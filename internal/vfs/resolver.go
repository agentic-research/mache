package vfs

import "github.com/agentic-research/mache/internal/graph"

// Resolver iterates a chain of VHandlers in registration order.
// The first handler whose Match returns true wins.
type Resolver struct {
	handlers []VHandler
}

// NewResolver creates a Resolver with the given handlers.
// Order matters — first match wins.
func NewResolver(handlers ...VHandler) *Resolver {
	return &Resolver{handlers: handlers}
}

// Resolve returns a VEntry for the path, or nil if no handler matches.
func (r *Resolver) Resolve(path string) *VEntry {
	for _, h := range r.handlers {
		if h.Match(path) {
			return h.Stat(path)
		}
	}
	return nil
}

// ReadContent delegates to the first matching handler.
func (r *Resolver) ReadContent(path string) ([]byte, bool) {
	for _, h := range r.handlers {
		if h.Match(path) {
			return h.ReadContent(path)
		}
	}
	return nil, false
}

// ListDir delegates to the first matching handler.
func (r *Resolver) ListDir(path string) ([]DirExtra, bool) {
	for _, h := range r.handlers {
		if h.Match(path) {
			return h.ListDir(path)
		}
	}
	return nil, false
}

// DirExtras collects extras from ALL handlers (not just first match).
// Each handler decides independently whether to inject entries.
func (r *Resolver) DirExtras(parentPath string, node *graph.Node) []DirExtra {
	var extras []DirExtra
	for _, h := range r.handlers {
		extras = append(extras, h.DirExtras(parentPath, node)...)
	}
	return extras
}

// Match returns true if any handler matches the path.
func (r *Resolver) Match(path string) bool {
	for _, h := range r.handlers {
		if h.Match(path) {
			return true
		}
	}
	return false
}
