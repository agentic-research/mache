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
// through this interface lets the engine stay walker-agnostic — both the
// tree-sitter (sitterMatch) and SQL (astMatch) backends implement it, so the
// engine no longer type-switches on the concrete walker to set these. The
// parity gate (TestASTQueryParity) asserts both backends return identical values.
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
// text — through this interface so it stays walker-agnostic: sitterMatch walks
// tree-sitter siblings, astMatch queries the _ast comment rows. Parity between
// the two is asserted by TestASTQueryParity (including a doc-comment fixture).
type DocScope interface {
	// ScopeSource returns the file's source bytes (for line/text computation).
	ScopeSource() []byte
	// DocRange returns the doc-extended start, the scope start, and the scope
	// end byte offsets. docStart == scopeStart when there are no contiguous
	// comment siblings. ok is false when the match has no @scope (e.g. a "$"
	// grouping match).
	DocRange() (docStart, scopeStart, scopeEnd uint32, ok bool)
}
