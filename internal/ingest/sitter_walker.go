package ingest

import (
	"fmt"
	"sync"
	"unsafe"

	sitter "github.com/smacker/go-tree-sitter"
)

// callQueryStr is the tree-sitter query pattern for extracting function calls.
// Extracted to a package-level const so it is defined once and reusable.
const callQueryStr = `
	(call_expression function: (identifier) @call)
	(call_expression function: (selector_expression field: (field_identifier) @call))
`

// SitterWalker implements Walker for Tree-sitter parsed code.
type SitterWalker struct {
	// callQueryCache caches compiled call-extraction queries keyed by *sitter.Language.
	// Assumption: sitter.Query objects are read-only during QueryCursor.Exec();
	// the cursor maintains its own iteration state, so sharing a compiled query
	// across sequential calls is safe. If tree-sitter's Go bindings ever mutate
	// the query during execution, this cache must be replaced with per-call compilation.
	callQueryCache sync.Map // *sitter.Language -> *sitter.Query
}

func NewSitterWalker() *SitterWalker {
	return &SitterWalker{}
}

// SitterRoot encapsulates the necessary context for querying a Tree-sitter tree.
// It includes the root node, the source code (for extracting content), and the language (for compiling the query).
type SitterRoot struct {
	Node   *sitter.Node
	Source []byte
	Lang   *sitter.Language
}

// Query implements Walker.
func (w *SitterWalker) Query(root any, selector string) ([]Match, error) {
	sr, ok := root.(SitterRoot)
	if !ok {
		// Also support *SitterRoot just in case
		if ptr, ok := root.(*SitterRoot); ok {
			sr = *ptr
		} else {
			return nil, fmt.Errorf("root must be SitterRoot, got %T", root)
		}
	}

	// "$" is a passthrough selector — returns the root itself with empty values.
	// Used for grouping nodes (like "functions", "types") that use literal names.
	if selector == "$" {
		return []Match{&sitterMatch{
			values: make(map[string]string),
			scope:  sr.Node,
			root:   sr,
		}}, nil
	}

	// Compile the query
	q, err := sitter.NewQuery([]byte(selector), sr.Lang)
	if err != nil {
		return nil, fmt.Errorf("invalid query '%s': %w", selector, err)
	}
	defer q.Close()

	// Execute query
	qc := sitter.NewQueryCursor()
	defer qc.Close()

	qc.Exec(q, sr.Node)

	var matches []Match
	for {
		m, ok := qc.NextMatch()
		if !ok {
			break
		}

		// Convert captures to map
		vals := make(map[string]string)
		captures := make(map[string]*sitter.Node)
		var scope *sitter.Node

		for _, c := range m.Captures {
			// Get capture name
			name := q.CaptureNameForId(c.Index)

			if name == "scope" {
				scope = c.Node
			}

			// Retain the raw sitter node for origin tracking
			captures[name] = c.Node

			// Extract content from source
			start := c.Node.StartByte()
			end := c.Node.EndByte()

			// Safety check
			if start < uint32(len(sr.Source)) && end <= uint32(len(sr.Source)) {
				vals[name] = string(sr.Source[start:end])
			} else {
				vals[name] = "" // Should not happen if source matches tree
			}
		}
		matches = append(matches, &sitterMatch{
			values:   vals,
			captures: captures,
			scope:    scope,
			root:     sr,
		})
	}

	return matches, nil
}

type sitterMatch struct {
	values   map[string]string
	captures map[string]*sitter.Node // raw nodes for origin tracking
	scope    *sitter.Node
	root     SitterRoot
}

// CaptureOrigin implements OriginProvider.
func (m *sitterMatch) CaptureOrigin(name string) (uint32, uint32, bool) {
	n, ok := m.captures[name]
	if !ok {
		return 0, 0, false
	}
	return n.StartByte(), n.EndByte(), true
}

// Values implements Match.
func (m *sitterMatch) Values() map[string]any {
	result := make(map[string]any, len(m.values))
	for k, v := range m.values {
		result[k] = v
	}
	return result
}

// Context implements Match.
func (m *sitterMatch) Context() any {
	if m.scope != nil {
		return SitterRoot{
			Node:   m.scope,
			Source: m.root.Source,
			Lang:   m.root.Lang,
		}
	}
	return nil
}

// getCallQuery returns a cached compiled query for call extraction, compiling
// it on first use for the given language. The compiled query is reused across
// all subsequent calls for the same language.
func (w *SitterWalker) getCallQuery(lang *sitter.Language) (*sitter.Query, error) {
	if cached, ok := w.callQueryCache.Load(lang); ok {
		return cached.(*sitter.Query), nil
	}
	q, err := sitter.NewQuery([]byte(callQueryStr), lang)
	if err != nil {
		return nil, err
	}
	// Store-or-load to handle concurrent first calls for the same language.
	// If another goroutine stored first, use theirs and close ours.
	actual, loaded := w.callQueryCache.LoadOrStore(lang, q)
	if loaded {
		q.Close()
		return actual.(*sitter.Query), nil
	}
	return q, nil
}

// ExtractCalls finds all function calls in the given node using a predefined query.
// The compiled query is cached per language to avoid recompilation on every call.
func (w *SitterWalker) ExtractCalls(root *sitter.Node, source []byte, lang *sitter.Language) ([]string, error) {
	q, err := w.getCallQuery(lang)
	if err != nil {
		return nil, fmt.Errorf("invalid call query: %w", err)
	}
	// Do NOT close q here — it is owned by the cache.

	qc := sitter.NewQueryCursor()
	defer qc.Close()

	qc.Exec(q, root)

	seen := make(map[string]bool)
	var calls []string

	for {
		m, ok := qc.NextMatch()
		if !ok {
			break
		}

		for _, c := range m.Captures {
			// Extract content
			start := c.Node.StartByte()
			end := c.Node.EndByte()
			if start < uint32(len(source)) && end <= uint32(len(source)) {
				// Use unsafe.String to check the seen map without allocating.
				// This avoids a heap allocation for tokens already encountered
				// (e.g., "Println" appearing hundreds of times). Only new,
				// unique tokens get a real string allocation via string().
				key := unsafe.String(&source[start], int(end-start))
				if !seen[key] {
					token := string(source[start:end])
					seen[token] = true
					calls = append(calls, token)
				}
			}
		}
	}
	return calls, nil
}
