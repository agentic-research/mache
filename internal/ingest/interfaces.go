package ingest

// Walker abstracts over JSONPath (Data) and Tree-sitter (Code).
// It provides a unified way to query a tree-like structure and extract values for path templating.
type Walker interface {
	// Query executes a selector (query) against the given root node and returns a list of matches.
	// The root node can be a *sitter.Node (for code) or a generic Go object (for data).
	Query(root any, selector string) ([]Match, error)
}

// Match represents a single result from a query.
// It provides a map of values that can be used to render path templates.
type Match interface {
	// Values returns the captured values.
	// For Tree-sitter, these are the named captures from the query (e.g., "res.type" -> "aws_s3_bucket").
	// For JSONPath, if the match is an object, its fields are returned as values.
	// If the match is a primitive, it might be returned under a default key (e.g., "value").
	Values() map[string]any

	// Context returns the underlying object/node to be used as the root for child queries.
	// For JSONPath, this is the matched object.
	// For Tree-sitter, this is the node captured as @scope (or similar convention).
	Context() any
}

// OriginProvider is an optional interface that Match implementations can satisfy
// to expose source byte ranges for write-back. Type-asserted in engine, not required by JSON walker.
type OriginProvider interface {
	CaptureOrigin(name string) (startByte, endByte uint32, ok bool)
}

// FileMeta is an optional interface a Match implements to expose per-file
// metadata the engine records as node Properties (lang, pkg). Reading it
// through this interface lets the engine stay walker-agnostic. Since ADR-0012
// step 4 removed in-process tree-sitter, astMatch (SQL over _ast) is the sole
// implementer; correctness is covered by `task test:ast`.
type FileMeta interface {
	// Lang is the source language ("go", "python", ...), or "" if unknown.
	Lang() string
	// PackageName is the file's package/namespace (Go package, etc.), or ""
	// when the language has no such concept or it can't be determined.
	PackageName() string
}

// DocScope is implemented by matches that can locate their @scope node's byte
// range, extended backward over contiguous preceding comment siblings (doc
// comments). The engine reads the `location` node property — and doc-comment
// text — through this interface so it stays walker-agnostic. astMatch (queries
// the _ast comment rows) is the sole implementer since in-process tree-sitter
// was removed; correctness is covered by `task test:ast`.
type DocScope interface {
	// ScopeSource returns the file's source bytes (for line/text computation).
	ScopeSource() []byte
	// DocRange returns the doc-extended start, the scope start, and the scope
	// end byte offsets. docStart == scopeStart when there are no contiguous
	// comment siblings. ok is false when the match has no @scope (e.g. a "$"
	// grouping match).
	DocRange() (docStart, scopeStart, scopeEnd uint32, ok bool)
}

// CallExtractor is implemented by matches that can extract the call tokens and
// address-refs WITHIN their @scope (per construct) for the callers/callees refs
// index. Since ADR-0012 step 4 removed in-process tree-sitter, astMatch is the
// sole implementer — it runs scope-prefixed SQL over _ast — and
// parentAwareMatch forwards. The interface is retained so the engine stays
// walker-agnostic. Correctness is covered by `task test:ast`.
type CallExtractor interface {
	// ScopeCalls returns deduplicated call tokens + typed address-refs found
	// within this match's scope. Per-construct, not whole-file.
	ScopeCalls() []string
}

// ASTScope is implemented by matches backed by a pre-parsed `_ast` table
// (astMatch) to expose the two identifiers a serve-time scoped+qualified
// callee query needs: the real `_ast`/`_source` key for the file, and the
// scope node id under which calls should be constrained (the same
// SourceID/ParentPrefix that ScopeCalls itself uses).
//
// The graph node id assigned to a construct (e.g.
// "cmd/functions/evalOrAbs") is NEITHER of these — it's a schema-rendered
// path, not an _ast key. Feeding it as source_id/scope id produces zero
// `_ast` rows every time (bead mache-fd9982 root cause: find_callees was
// silently broken on the serve/mount path). The engine persists these two
// identifiers onto the construct's graph node (Properties
// "ast_source_id"/"ast_scope_id") so GetCallees can recover them without
// re-deriving anything from the node id.
type ASTScope interface {
	// ASTSourceID returns the real `_ast`/`_source` key for this match's file.
	ASTSourceID() string
	// ASTScopeID returns the `_ast` scope node id (ctx.ParentPrefix) this
	// construct's calls are constrained to.
	ASTScopeID() string
}
