package ingest

import (
	"bytes"
	"fmt"
	"sync"
	"unsafe"

	"github.com/agentic-research/mache/internal/graph"
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

// contextQueryRegistry stores language-specific context extraction queries.
var contextQueryRegistry sync.Map // string (language name) -> string

// qualifiedCallQueryRegistry stores language-specific queries that capture
// both @call and @pkg for qualified call resolution (e.g., auth.Validate).
var qualifiedCallQueryRegistry sync.Map // string (language name) -> string

// fileLevelRefQueryRegistry stores per-language queries run against the
// FILE root (not function bodies). The use case is patterns whose
// captures live outside any function_declaration — Go's top-level
// `var serveCmd = &cobra.Command{ RunE: runServe }` is the
// motivating case (mache-02r9). Per-scope ExtractCalls would never
// see runServe because the keyed_element is in a top-level
// var_declaration, not a function body.
var fileLevelRefQueryRegistry sync.Map // string (language name) -> string

// RegisterRefQuery registers a reference extraction query for a specific language.
// This should be called during initialization.
func RegisterRefQuery(langName, query string) {
	refQueryRegistry.Store(langName, query)
}

// RegisterFileLevelRefQuery registers a reference extraction query
// that runs once per FILE (against the root node), not per function
// scope. The captures supplement per-scope ExtractCalls — typical use:
// matching identifiers used as struct field values in top-level
// composite literals.
func RegisterFileLevelRefQuery(langName, query string) {
	fileLevelRefQueryRegistry.Store(langName, query)
}

// RegisterContextQuery registers a context extraction query for a specific language.
// This should be called during initialization.
func RegisterContextQuery(langName, query string) {
	contextQueryRegistry.Store(langName, query)
}

// RegisterQualifiedCallQuery registers a call extraction query that captures
// both @call (function name) and @pkg (package qualifier) for a language.
func RegisterQualifiedCallQuery(langName, query string) {
	qualifiedCallQueryRegistry.Store(langName, query)
}

// schemaQueryKey identifies a compiled tree-sitter query by its selector text
// and target language. Used as cache key for schema selector compilation.
type schemaQueryKey struct {
	selector string
	lang     uintptr // *sitter.Language pointer identity
}

// SitterWalker implements Walker for Tree-sitter parsed code.
type SitterWalker struct {
	// callQueryCache caches compiled call-extraction queries keyed by language name.
	callQueryCache sync.Map // string (language name) -> *sitter.Query
	// contextQueryCache caches compiled context queries.
	contextQueryCache sync.Map // string (language name) -> *sitter.Query
	// qualifiedCallQueryCache caches compiled qualified call queries.
	qualifiedCallQueryCache sync.Map // string (language name) -> *sitter.Query
	// addressRefQueryCache caches compiled address ref queries keyed by
	// (language name, scheme) pair.
	addressRefQueryCache sync.Map // addressRefQueryCacheKey -> *sitter.Query
	// schemaQueryCache caches compiled schema selector queries keyed by
	// (selector, language) pair. Schema selectors are the same across all
	// files of the same language, so caching avoids recompilation on every file.
	schemaQueryCache sync.Map // schemaQueryKey -> *sitter.Query
	// fileLevelRefQueryCache caches compiled file-level ref queries.
	fileLevelRefQueryCache sync.Map // string (language name) -> *sitter.Query
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
			w:      w,
		}}, nil
	}

	// Compile the query (cached per selector+language pair).
	q, err := w.getSchemaQuery(sr.Lang, selector)
	if err != nil {
		return nil, fmt.Errorf("invalid query '%s': %w", selector, err)
	}
	// Do NOT close q here — it is owned by the cache.

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
			w:        w,
		})
	}

	return matches, nil
}

type sitterMatch struct {
	values   map[string]string
	captures map[string]*sitter.Node // raw nodes for origin tracking
	scope    *sitter.Node
	root     SitterRoot
	w        *SitterWalker // for per-scope ScopeCalls (uses the query cache)
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

// Lang implements FileMeta.
func (m *sitterMatch) Lang() string { return m.root.LangName }

// PackageName implements FileMeta. Go-only today (mirrors the engine's prior
// SitterWalker-gated behavior); other languages return "".
func (m *sitterMatch) PackageName() string {
	if m.root.LangName == "go" && m.root.FileRoot != nil {
		return extractGoPackageName(m.root.FileRoot, m.root.Source, m.root.Lang)
	}
	return ""
}

// ScopeSource implements DocScope.
func (m *sitterMatch) ScopeSource() []byte { return m.root.Source }

// DocRange implements DocScope — walks backward from the @scope capture over
// contiguous preceding comment siblings (the original extractDocComments logic).
func (m *sitterMatch) DocRange() (docStart, scopeStart, scopeEnd uint32, ok bool) {
	scopeNode := m.captures["scope"]
	if scopeNode == nil {
		return 0, 0, 0, false
	}
	scopeStart = scopeNode.StartByte()
	scopeEnd = scopeNode.EndByte()
	docStart = scopeStart

	n := scopeNode
	prev := n.PrevSibling()
	for prev != nil && prev.Type() == "comment" {
		// Adjacency: <= 2 bytes gap (allow \n or \n\n).
		if int(n.StartByte())-int(prev.EndByte()) <= 2 {
			docStart = prev.StartByte()
			n = prev
			prev = prev.PrevSibling()
		} else {
			break
		}
	}
	return docStart, scopeStart, scopeEnd, true
}

// ScopeCalls implements CallExtractor — calls + address-refs within this scope.
func (m *sitterMatch) ScopeCalls() []string {
	if m.w == nil || m.scope == nil {
		return nil
	}
	calls, _ := m.w.ExtractCalls(m.scope, m.root.Source, m.root.Lang, m.root.LangName)
	if addr, err := m.w.ExtractAddressRefs(m.scope, m.root.Source, m.root.Lang, m.root.LangName); err == nil {
		calls = append(calls, addr...)
	}
	return calls
}

// getSchemaQuery returns a cached compiled query for schema selector execution.
// The compiled query is reused across all files of the same language, avoiding
// recompilation of the same S-expression on every file (e.g., 50K files × 20
// selectors = 1M compilations reduced to ~20).
func (w *SitterWalker) getSchemaQuery(lang *sitter.Language, selector string) (*sitter.Query, error) {
	key := schemaQueryKey{selector: selector, lang: uintptr(unsafe.Pointer(lang))}
	if cached, ok := w.schemaQueryCache.Load(key); ok {
		return cached.(*sitter.Query), nil
	}

	q, err := sitter.NewQuery([]byte(selector), lang)
	if err != nil {
		return nil, err
	}
	actual, loaded := w.schemaQueryCache.LoadOrStore(key, q)
	if loaded {
		q.Close()
		return actual.(*sitter.Query), nil
	}
	return q, nil
}

// Close releases all cached compiled queries. Call when the SitterWalker is
// no longer needed (e.g., after ingestion completes).
func (w *SitterWalker) Close() {
	w.schemaQueryCache.Range(func(_, v any) bool {
		v.(*sitter.Query).Close()
		return true
	})
	w.callQueryCache.Range(func(_, v any) bool {
		v.(*sitter.Query).Close()
		return true
	})
	w.contextQueryCache.Range(func(_, v any) bool {
		v.(*sitter.Query).Close()
		return true
	})
	w.qualifiedCallQueryCache.Range(func(_, v any) bool {
		v.(*sitter.Query).Close()
		return true
	})
	w.addressRefQueryCache.Range(func(_, v any) bool {
		v.(*sitter.Query).Close()
		return true
	})
	w.fileLevelRefQueryCache.Range(func(_, v any) bool {
		v.(*sitter.Query).Close()
		return true
	})
}

// getFileLevelRefQuery returns a cached compiled query for file-level
// ref extraction. Returns nil (no error) when no query is registered
// for the language — caller treats nil as 'no file-level extraction
// for this lang' and skips.
func (w *SitterWalker) getFileLevelRefQuery(lang *sitter.Language, langName string) (*sitter.Query, error) {
	if cached, ok := w.fileLevelRefQueryCache.Load(langName); ok {
		return cached.(*sitter.Query), nil
	}
	val, ok := fileLevelRefQueryRegistry.Load(langName)
	if !ok {
		return nil, nil
	}
	q, err := sitter.NewQuery([]byte(val.(string)), lang)
	if err != nil {
		return nil, err
	}
	actual, loaded := w.fileLevelRefQueryCache.LoadOrStore(langName, q)
	if loaded {
		q.Close()
		return actual.(*sitter.Query), nil
	}
	return q, nil
}

// ExtractFileLevelRefs runs the file-level ref query against the
// FILE root and returns matched identifier text (deduped). Used to
// catch identifiers in positions that per-scope ExtractCalls can't
// see — e.g. function references in top-level cobra var declarations
// (mache-02r9). Returns nil when no file-level query is registered
// for the language.
func (w *SitterWalker) ExtractFileLevelRefs(root *sitter.Node, source []byte, lang *sitter.Language, langName string) ([]string, error) {
	q, err := w.getFileLevelRefQuery(lang, langName)
	if err != nil {
		return nil, fmt.Errorf("invalid file-level ref query: %w", err)
	}
	if q == nil {
		return nil, nil
	}
	// Don't close q — owned by the cache.
	qc := sitter.NewQueryCursor()
	defer qc.Close()
	qc.Exec(q, root)

	seen := make(map[string]bool)
	var refs []string
	for {
		m, ok := qc.NextMatch()
		if !ok {
			break
		}
		m = qc.FilterPredicates(m, source)
		for _, c := range m.Captures {
			start := c.Node.StartByte()
			end := c.Node.EndByte()
			if start < uint32(len(source)) && end <= uint32(len(source)) {
				key := unsafe.String(&source[start], int(end-start))
				if !seen[key] {
					token := string(source[start:end])
					seen[token] = true
					refs = append(refs, token)
				}
			}
		}
	}
	return refs, nil
}

// getContextQuery returns a cached compiled query for context extraction.
func (w *SitterWalker) getContextQuery(lang *sitter.Language, langName string) (*sitter.Query, error) {
	if cached, ok := w.contextQueryCache.Load(langName); ok {
		return cached.(*sitter.Query), nil
	}

	qStr := ""
	if val, ok := contextQueryRegistry.Load(langName); ok {
		qStr = val.(string)
	}

	if qStr == "" {
		return nil, nil // No query for this language
	}

	q, err := sitter.NewQuery([]byte(qStr), lang)
	if err != nil {
		return nil, err
	}

	actual, loaded := w.contextQueryCache.LoadOrStore(langName, q)
	if loaded {
		q.Close()
		return actual.(*sitter.Query), nil
	}
	return q, nil
}

// ExtractContext finds package-level context nodes.
func (w *SitterWalker) ExtractContext(root *sitter.Node, source []byte, lang *sitter.Language, langName string) ([]byte, error) {
	q, err := w.getContextQuery(lang, langName)
	if err != nil {
		return nil, fmt.Errorf("invalid context query: %w", err)
	}
	if q == nil {
		return nil, nil // Not supported for this language
	}
	// Do NOT close q here — it is owned by the cache.

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

// goImportQuery is the tree-sitter query for Go import declarations.
// Captures alias (optional) and path for each import spec.
const goImportQuery = `
(import_spec
  name: (package_identifier)? @alias
  path: (interpreted_string_literal) @path)
`

// ExtractGoImports extracts structured import mappings from a Go AST.
// Returns a map of alias → import path (e.g., "fmt" → "fmt", "mypkg" → "github.com/foo/bar").
// For unaliased imports, the alias is the last path segment.
func (w *SitterWalker) ExtractGoImports(root *sitter.Node, source []byte, lang *sitter.Language) map[string]string {
	q, err := w.getGoImportQuery(lang)
	if err != nil || q == nil {
		return nil
	}

	qc := sitter.NewQueryCursor()
	defer qc.Close()
	qc.Exec(q, root)

	imports := make(map[string]string)
	for {
		m, ok := qc.NextMatch()
		if !ok {
			break
		}

		var alias, path string
		for _, c := range m.Captures {
			name := q.CaptureNameForId(c.Index)
			text := c.Node.Content(source)
			switch name {
			case "alias":
				alias = text
			case "path":
				// Strip surrounding quotes from interpreted_string_literal
				if len(text) >= 2 && text[0] == '"' {
					text = text[1 : len(text)-1]
				}
				path = text
			}
		}

		if path == "" || alias == "_" || alias == "." {
			continue
		}
		if alias == "" {
			// Default alias is last path segment
			parts := bytes.Split([]byte(path), []byte("/"))
			alias = string(parts[len(parts)-1])
		}
		imports[alias] = path
	}

	return imports
}

var goImportQueryCache sync.Map // *sitter.Language → *sitter.Query

func (w *SitterWalker) getGoImportQuery(lang *sitter.Language) (*sitter.Query, error) {
	ptr := uintptr(unsafe.Pointer(lang))
	if cached, ok := goImportQueryCache.Load(ptr); ok {
		return cached.(*sitter.Query), nil
	}
	q, err := sitter.NewQuery([]byte(goImportQuery), lang)
	if err != nil {
		return nil, err
	}
	actual, loaded := goImportQueryCache.LoadOrStore(ptr, q)
	if loaded {
		q.Close()
		return actual.(*sitter.Query), nil
	}
	return q, nil
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

// getQualifiedCallQuery returns a cached compiled query for qualified call
// extraction. Falls back to nil if no qualified query is registered.
func (w *SitterWalker) getQualifiedCallQuery(lang *sitter.Language, langName string) (*sitter.Query, error) {
	if cached, ok := w.qualifiedCallQueryCache.Load(langName); ok {
		return cached.(*sitter.Query), nil
	}

	qStr := ""
	if val, ok := qualifiedCallQueryRegistry.Load(langName); ok {
		qStr = val.(string)
	}
	if qStr == "" {
		return nil, nil // No qualified query for this language
	}

	q, err := sitter.NewQuery([]byte(qStr), lang)
	if err != nil {
		return nil, err
	}
	actual, loaded := w.qualifiedCallQueryCache.LoadOrStore(langName, q)
	if loaded {
		q.Close()
		return actual.(*sitter.Query), nil
	}
	return q, nil
}

// ExtractQualifiedCalls finds all function calls with optional package qualifiers.
// For languages with a registered qualified call query, returns QualifiedCall with
// both Token and Qualifier. For others, falls back to ExtractCalls (bare tokens).
func (w *SitterWalker) ExtractQualifiedCalls(root *sitter.Node, source []byte, lang *sitter.Language, langName string) ([]graph.QualifiedCall, error) {
	q, err := w.getQualifiedCallQuery(lang, langName)
	if err != nil {
		return nil, fmt.Errorf("invalid qualified call query: %w", err)
	}

	// Fall back to regular ExtractCalls if no qualified query registered
	if q == nil {
		bare, err := w.ExtractCalls(root, source, lang, langName)
		if err != nil {
			return nil, err
		}
		result := make([]graph.QualifiedCall, len(bare))
		for i, token := range bare {
			result[i] = graph.QualifiedCall{Token: token}
		}
		return result, nil
	}
	// Do NOT close q here — it is owned by the cache.

	qc := sitter.NewQueryCursor()
	defer qc.Close()
	qc.Exec(q, root)

	seen := make(map[string]bool)
	var calls []graph.QualifiedCall

	for {
		m, ok := qc.NextMatch()
		if !ok {
			break
		}

		m = qc.FilterPredicates(m, source)
		if len(m.Captures) == 0 {
			continue
		}

		var callToken, pkgQualifier string
		for _, c := range m.Captures {
			name := q.CaptureNameForId(c.Index)
			start := c.Node.StartByte()
			end := c.Node.EndByte()
			if start < uint32(len(source)) && end <= uint32(len(source)) {
				switch name {
				case "call":
					callToken = string(source[start:end])
				case "pkg":
					pkgQualifier = string(source[start:end])
				}
			}
		}

		if callToken == "" {
			continue
		}

		key := callToken
		if pkgQualifier != "" {
			key = pkgQualifier + "." + callToken
		}

		if !seen[key] {
			seen[key] = true
			calls = append(calls, graph.QualifiedCall{
				Token:     callToken,
				Qualifier: pkgQualifier,
			})
		}
	}

	return calls, nil
}
