package graph

import "database/sql"

// The optional capability interfaces a backend may implement beyond [Graph].
//
// [Graph] is the segment-and-piece layer: walk directories, read a node,
// follow callers and callees. It is deliberately small, because every backend
// must satisfy all of it. But browsing source is a ladder — segments to pieces
// to SYMBOLS — and the top rung is where a reader stops guessing and asks a
// direct question: "where is Foo defined?", "what matches *Handler?".
//
// Those answers exist on the SQL-backed graphs and not on the in-memory ones,
// so they cannot join [Graph] without forcing every backend to fake them. The
// Go answer is an optional interface, discovered by type assertion.
//
// They are declared HERE, exported, for one reason: an interface a consumer
// cannot name is an interface a consumer must re-derive. These previously
// lived unexported in package cmd, so the only way to reach them from outside
// was to hand-write the method set:
//
//	ld, ok := g.(interface{ LookupDef(string) []string })
//
// mache's own resolve tests did exactly that. That spelling is invisible to
// godoc, breaks silently when a signature changes, and has to be rediscovered
// by every consumer — the browsing cost this package exists to remove.
//
// Assert on them directly; each returns its zero value when the backend does
// not implement it, so a caller degrades rather than fails:
//
//	if dl, ok := g.(graph.DefsLookuper); ok {
//	    ids := dl.LookupDef("Foo")
//	}

// DefsLookuper answers "where is this exact symbol defined?" — the cheapest
// question on the symbol rung, and the one worth asking first. Backends
// implement it instead of [DefsMapper] wherever a single lookup can avoid
// snapshotting the whole index.
//
// Returns the node IDs declaring token, or nil when nothing declares it.
type DefsLookuper interface {
	LookupDef(token string) []string
}

// DefsSearcher answers "what symbols look like this?" using SQL LIKE syntax
// ('%' matches any run, '_' a single character), capped at limit. The step
// between knowing a name and knowing only its shape.
type DefsSearcher interface {
	SearchDefs(pattern string, limit int) map[string][]string
}

// DefsMapper exposes the whole symbol index as token -> node IDs.
//
// Prefer [DefsLookuper] or [DefsSearcher]: this copies the entire index, which
// is the expensive way to answer a question about one symbol. It earns its
// keep for whole-index work — building an overview, detecting duplicates.
type DefsMapper interface {
	DefsMap() map[string][]string
}

// RefsMapper exposes the reference index as node ID -> referenced node IDs,
// the edge set community detection and impact analysis run over.
type RefsMapper interface {
	RefsMap() map[string][]string
}

// RefsQuerier runs SQL directly against the backing database — the escape
// hatch for questions the typed accessors above do not cover, including the
// `_ast` and `_lsp*` tables ley-line-open produces.
//
// The tables available depend on who produced the .db; see
// docs/ARCHITECTURE.md for the capability matrix.
type RefsQuerier interface {
	QueryRefs(query string, args ...any) (*sql.Rows, error)
}

// DBPathProvider reports the path of the .db backing this graph, or "" when
// the backend has no file behind it.
//
// "" is a documented sentinel meaning "no source", not an error: consumers
// that locate sibling artifacts by convention (ley-line-open writes a
// `.bindings.capnp` reference log next to the .db) must treat it as "nothing
// to consult" and carry on.
type DBPathProvider interface {
	DBPath() string
}

// MountPrefixer reports which mount a node ID came from, or "" for the base
// graph — the disambiguation a composite graph needs once a single query can
// answer across more than one repository.
type MountPrefixer interface {
	MountPrefixOf(id string) string
}

// RefResolver answers the two questions a reference-walking consumer asks that
// [Graph] does not: where is this reference in the file, and what definition
// does it point at.
//
// Both were previously reachable only by hand-writing SQL against node_refs,
// _ast and _imports — the join every consumer rewrote, and the bare-token
// matching that cannot tell a local Join from filepath.Join. See
// [RefTarget.Resolution] for why "did not resolve" is four distinct answers
// rather than a boolean.
type RefResolver interface {
	// RefRangeOf reports where a reference node sits, or nil when the
	// projection carries no spans.
	RefRangeOf(nodeID string) (*RefRange, error)
	// ResolveRef reports what a reference points at, classified.
	ResolveRef(nodeID string) (*RefTarget, error)
}

// DefsNodeLookuper is [DefsLookuper]'s node-returning twin, for consumers that
// want the definition itself rather than its id.
//
// Both exist deliberately. Ids are the right answer when the caller only needs
// identity — resolving nodes it will discard is waste — but returning ids from
// one accessor while [Graph.GetCallers] and [Graph.GetCallees] return nodes
// left every consumer writing the same conversion. That asymmetry is what drove
// downstream wrappers around this surface.
type DefsNodeLookuper interface {
	LookupDefNodes(token string) ([]*Node, error)
}
