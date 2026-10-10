package fixturedb

import (
	"maps"
	"slices"
)

// The two projection-v4 dialects: ley-line-open's parse output and mache's own
// SQLiteWriter projection. They share a `nodes` table and an `_ast` table and
// differ in node_defs / node_refs, in which ley-line-owned tables exist, and in
// whether a parse tree carries merkle child lists.

// leylineDialect models ley-line-open at the pinned version (projection-v4).
type leylineDialect struct{}

func (leylineDialect) schema(b *Builder) []string {
	stmts := tableDDL(leylineTableOrder, leylineTables)
	stmts = append(stmts, tableDDL(slices.Sorted(maps.Keys(leylineIndexes)), leylineIndexes)...)
	return append(stmts, b.lspDDL()...)
}

// Ley-line always writes node_content.
func (leylineDialect) hasNodeContent(*Builder) bool { return true }

func (leylineDialect) emitNodes(e *emitter) { e.insertV4Nodes() }

func (leylineDialect) emitSymbols(e *emitter) {
	emitLeylineDefs(e)
	emitLeylineRefs(e)
}

func emitLeylineDefs(e *emitter) {
	for _, d := range e.b.defs {
		h := e.subtree(d.subtree, string(d.kind), d.token)
		e.exec(`INSERT INTO node_defs (token, node_id, source_id, container_node_id, canonical_kind, node_hash)
			VALUES (?, ?, ?, ?, ?, ?)`,
			d.token, string(d.nodeID), string(e.b.sourceOf(d.nodeID)),
			nullIfEmpty(string(d.container)), nullIfEmpty(string(d.kind)), h)
	}
}

func emitLeylineRefs(e *emitter) {
	for _, r := range e.b.refs {
		h := e.subtree(r.subtree, "call_expression", r.token)
		e.exec(`INSERT INTO node_refs (token, node_id, source_id, container_node_id, qualifier, node_hash)
			VALUES (?, ?, ?, ?, ?, ?)`,
			r.token, string(r.at), string(e.b.sourceOf(r.from)), string(r.from),
			nullIfEmpty(r.qualifier), h)
	}
}

func (leylineDialect) emitAST(e *emitter) { e.emitNodeChildren(e.insertV4AST()) }

func (leylineDialect) emitSources(e *emitter) { e.insertV4Sources() }

func (leylineDialect) emitImports(e *emitter) {
	for _, im := range e.b.imports {
		e.exec(`INSERT INTO _imports (alias, path, source_id) VALUES (?, ?, ?)`,
			im.alias, im.importPath, string(im.source))
	}
}

// standaloneDialect models mache's own projection (internal/ingest.SQLiteWriter).
type standaloneDialect struct{}

// The ley-line-owned tables are created only when the fixture declares rows
// for them. ensureCanonicalViews PROBES for `_ast`: creating it
// unconditionally would flip every Standalone fixture onto the v_test_nodes arm
// that real mache .db files never reach.
func (standaloneDialect) schema(b *Builder) []string {
	stmts := tableDDL(standaloneTableOrder, standaloneTables)
	stmts = append(stmts, tableDDL(slices.Sorted(maps.Keys(standaloneIndexes)), standaloneIndexes)...)
	stmts = append(stmts, tableDDL(slices.Sorted(maps.Keys(standaloneViews)), standaloneViews)...)
	// The cache-hydration path (cmd/cache.go) materialises ley-line's parse
	// output onto a mache-projection .db. Model it only when the fixture
	// actually declares such rows.
	if len(b.ast) > 0 {
		stmts = append(stmts, leylineTables["node_content"], leylineTables["_ast"])
	}
	if len(b.sources) > 0 {
		stmts = append(stmts, leylineTables["_source"])
	}
	return append(stmts, b.lspDDL()...)
}

// A Standalone fixture has node_content only when it modelled the
// cache-hydration path.
func (standaloneDialect) hasNodeContent(b *Builder) bool { return len(b.ast) > 0 }

func (standaloneDialect) emitNodes(e *emitter) { e.insertV4Nodes() }

// In the mache projection a def and a ref are the same thing: a (token,
// construct) pair in a two-column table whose primary key collapses
// duplicates.
//
// Everything the spec says about a def's kind, container and content identity
// is DROPPED, which is the honest outcome, not a lossy shortcut. A ref has no
// site column and no qualifier column: the ENCLOSING CONSTRUCT is what lands in
// node_id.
func (standaloneDialect) emitSymbols(e *emitter) {
	for _, m := range e.b.mentions() {
		e.exec(`INSERT OR IGNORE INTO `+m.table+` (token, node_id) VALUES (?, ?)`, m.token, string(m.node))
	}
}

// The hydrated `_ast` carries no merkle child lists.
func (standaloneDialect) emitAST(e *emitter) { e.insertV4AST() }

func (standaloneDialect) emitSources(e *emitter) { e.insertV4Sources() }

// The mache projection has no _imports table; [Builder.Import] documents that
// the call is dropped here.
func (standaloneDialect) emitImports(*emitter) {}

// mention is one def or ref as the mache projection records it.
type mention struct {
	table, token string
	node         ConstructID
}

// mentions lists every def and ref as a mention: a def at the construct that
// defines it, a ref at the construct it is made FROM.
func (b *Builder) mentions() []mention {
	out := make([]mention, 0, len(b.defs)+len(b.refs))
	for _, d := range b.defs {
		out = append(out, mention{table: "node_defs", token: d.token, node: d.nodeID})
	}
	for _, r := range b.refs {
		out = append(out, mention{table: "node_refs", token: r.token, node: r.from})
	}
	return out
}

// tableDDL returns the statements for names, in order.
func tableDDL(names []string, from map[string]string) []string {
	stmts := make([]string, 0, len(names))
	for _, n := range names {
		stmts = append(stmts, from[n])
	}
	return stmts
}
