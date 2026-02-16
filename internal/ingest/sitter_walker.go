package ingest

import (
	"bytes"
	"fmt"
	"sync"
	"unsafe"

	sitter "github.com/smacker/go-tree-sitter"
)

// defaultCallQuery is the tree-sitter query pattern for extracting function calls
// in C-style languages (Go, JS, TS).
const defaultCallQuery = `
	(call_expression function: (identifier) @call)
	(call_expression function: (selector_expression field: (field_identifier) @call))
`

// refQueryRegistry stores language-specific reference extraction queries.
var refQueryRegistry sync.Map // string (language name) -> string

// RegisterRefQuery registers a reference extraction query for a specific language.
// This should be called during initialization.
func RegisterRefQuery(langName string, query string) {
	refQueryRegistry.Store(langName, query)
}

// SitterWalker implements Walker for Tree-sitter parsed code.
type SitterWalker struct {
	// callQueryCache caches compiled call-extraction queries keyed by language name.
	callQueryCache sync.Map // string (language name) -> *sitter.Query
}

func NewSitterWalker() *SitterWalker {
	return &SitterWalker{}
}

// SitterRoot encapsulates the necessary context for querying a Tree-sitter tree.
// It includes the root node, the source code (for extracting content), and the language (for compiling the query).
type SitterRoot struct {
	Node     *sitter.Node
	FileRoot *sitter.Node // The top-level file node (for global context)
	Source   []byte
	Lang     *sitter.Language
	LangName string // "go", "python", "hcl", etc.
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

	// Ensure FileRoot is set (if this is the top level)
	if sr.FileRoot == nil {
		sr.FileRoot = sr.Node
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

		// Enforce #eq? / #not-eq? predicates in the query.
		// When no predicates exist this is a no-op (copies captures through unchanged).
		m = qc.FilterPredicates(m, sr.Source)
		if len(m.Captures) == 0 {
			continue
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

// GetCaptureNode returns the raw tree-sitter node for a given capture name.
// This allows access to the AST for advanced processing (e.g. extending range).
func (m *sitterMatch) GetCaptureNode(name string) *sitter.Node {
	return m.captures[name]
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
			Node:     m.scope,
			FileRoot: m.root.FileRoot,
			Source:   m.root.Source,
			Lang:     m.root.Lang,
			LangName: m.root.LangName,
		}
	}
	return nil
}

// contextQueryStr captures package-level definitions for the context file.
const contextQueryStr = `
	; (package_clause) @ctx
	(import_declaration) @ctx
	(const_declaration) @ctx
	(var_declaration) @ctx
	(type_declaration) @ctx
`

// getContextQuery returns a cached compiled query for context extraction.
func (w *SitterWalker) getContextQuery(lang *sitter.Language) (*sitter.Query, error) {
	// Re-use callQueryCache mechanism or add a new cache?
	// For simplicity, let's just compile it. Performance optimization can come later.
	return sitter.NewQuery([]byte(contextQueryStr), lang)
}

// ExtractContext finds package-level context nodes.
func (w *SitterWalker) ExtractContext(root *sitter.Node, source []byte, lang *sitter.Language) ([]byte, error) {
	q, err := w.getContextQuery(lang)
	if err != nil {
		return nil, fmt.Errorf("invalid context query: %w", err)
	}
	defer q.Close()

	qc := sitter.NewQueryCursor()
	defer qc.Close()

	qc.Exec(q, root)

	var buf bytes.Buffer
	seen := make(map[uint32]bool) // avoid duplicates if multiple captures match same node

	for {
		m, ok := qc.NextMatch()
		if !ok {
			break
		}

		for _, c := range m.Captures {
			if seen[c.Node.StartByte()] {
				continue
			}
			seen[c.Node.StartByte()] = true

			start := c.Node.StartByte()
			end := c.Node.EndByte()
			if start < uint32(len(source)) && end <= uint32(len(source)) {
				buf.Write(source[start:end])
				buf.WriteString("\n\n")
			}
		}
	}
	return buf.Bytes(), nil
}

// getCallQuery returns a cached compiled query for call extraction, compiling
// it on first use for the given language. The compiled query is reused across
// all subsequent calls for the same language.
func (w *SitterWalker) getCallQuery(lang *sitter.Language, langName string) (*sitter.Query, error) {
	if cached, ok := w.callQueryCache.Load(langName); ok {
		return cached.(*sitter.Query), nil
	}

	// Lookup query string
	qStr := defaultCallQuery
	if val, ok := refQueryRegistry.Load(langName); ok {
		qStr = val.(string)
	}

	q, err := sitter.NewQuery([]byte(qStr), lang)
	if err != nil {
		return nil, err
	}
	// Store-or-load to handle concurrent first calls for the same language.
	// If another goroutine stored first, use theirs and close ours.
	actual, loaded := w.callQueryCache.LoadOrStore(langName, q)
	if loaded {
		q.Close()
		return actual.(*sitter.Query), nil
	}
	return q, nil
}

// ExtractCalls finds all function calls in the given node using a predefined query.
// The compiled query is cached per language to avoid recompilation on every call.
func (w *SitterWalker) ExtractCalls(root *sitter.Node, source []byte, lang *sitter.Language, langName string) ([]string, error) {
	q, err := w.getCallQuery(lang, langName)
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

		// Enforce #eq? / #not-eq? predicates in the query.
		m = qc.FilterPredicates(m, source)
		if len(m.Captures) == 0 {
			continue
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
