package vfs

import (
	"sync"

	"github.com/agentic-research/mache/internal/graph"
)

// Resolver iterates a chain of VHandlers in registration order.
// The first handler whose Match returns true wins.
type Resolver struct {
	handlers []VHandler

	// Typed references for post-construction configuration.
	// Backends call SetPromptContent/EnableQuery/SetWritable
	// instead of holding direct handler pointers.
	promptH *PromptHandler
	queryH  *QueryHandler
	diagH   *DiagnosticsHandler
}

// NewResolver creates a Resolver with the given handlers.
// Order matters — first match wins.
func NewResolver(handlers ...VHandler) *Resolver {
	return &Resolver{handlers: handlers}
}

// NewDefaultResolver builds the standard handler chain for both FUSE and NFS
// backends. Callers configure behaviour via SetPromptContent, EnableQuery,
// and SetWritable rather than accessing individual handlers.
func NewDefaultResolver(g graph.Graph, schemaJSON []byte) *Resolver {
	promptH := &PromptHandler{}
	queryH := &QueryHandler{}
	diagH := &DiagnosticsHandler{DiagStatus: &sync.Map{}}
	schemaH := &SchemaHandler{Content: schemaJSON}
	contextH := &ContextHandler{Graph: g}
	locationH := &LocationHandler{Graph: g}
	callersH := &CallersHandler{Graph: g}
	calleesH := &CalleesHandler{Graph: g}

	// Order matters: query before callers/callees (both can have "/" paths).
	r := NewResolver(
		schemaH, promptH, queryH, diagH, contextH, locationH, callersH, calleesH,
	)
	r.promptH = promptH
	r.queryH = queryH
	r.diagH = diagH
	return r
}

// SetPromptContent sets the content for the /PROMPT.txt virtual file.
func (r *Resolver) SetPromptContent(content []byte) {
	if r.promptH != nil {
		r.promptH.Content = content
	}
}

// EnableQuery marks the /.query/ magic directory as active.
func (r *Resolver) EnableQuery() {
	if r.queryH != nil {
		r.queryH.Enabled = true
	}
}

// SetWritable enables _diagnostics/ virtual dirs and wires the status map.
func (r *Resolver) SetWritable(writable bool, diagStatus *sync.Map) {
	if r.diagH != nil {
		r.diagH.Writable = writable
		if diagStatus != nil {
			r.diagH.DiagStatus = diagStatus
		}
	}
}

// Resolve returns a VEntry for the path, or nil if no handler matches.
// When a handler matches but Stat returns nil (e.g., a node named "context"
// that has no virtual content), resolution continues to the next handler
// so the path can fall through to the graph lookup.
func (r *Resolver) Resolve(path string) *VEntry {
	for _, h := range r.handlers {
		if h.Match(path) {
			if entry := h.Stat(path); entry != nil {
				return entry
			}
		}
	}
	return nil
}

// ReadContent delegates to the first matching handler that returns content.
func (r *Resolver) ReadContent(path string) ([]byte, bool) {
	for _, h := range r.handlers {
		if h.Match(path) {
			if data, ok := h.ReadContent(path); ok {
				return data, true
			}
		}
	}
	return nil, false
}

// ListDir delegates to the first matching handler that returns entries.
func (r *Resolver) ListDir(path string) ([]DirExtra, bool) {
	for _, h := range r.handlers {
		if h.Match(path) {
			if entries, ok := h.ListDir(path); ok {
				return entries, true
			}
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
