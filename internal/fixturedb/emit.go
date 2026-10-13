package fixturedb

import (
	"database/sql"
	"encoding/hex"
	"path"
	"slices"
	"strings"

	"github.com/agentic-research/mache/internal/sqlintro"
)

// Emission: the ONE place a spec becomes columns.
//
// Every producer difference lives in the producer's [dialect], never in a
// caller and never in a `CREATE TABLE` a test wrote. That is what makes "which
// producer did this test mean?" a question with an answer. This file holds the
// row writers the dialects share.

// emitter carries the per-build state the row writers share.
type emitter struct {
	// derivedParent is true when the producer's nodes.parent_id is a GENERATED
	// column, which ley-line-open made it in projection-v4 (v0.19.0). Naming a
	// generated column in an INSERT is rejected at PREPARE time, so the column
	// LIST has to differ — the value does not, since the derivation reproduces
	// exactly what this emitter would have written.
	derivedParent bool
	b             *Builder
	db            *sql.Tx
	// hasNodeContent reports whether this fixture has a node_content table to
	// point node_hash values at. Ley-line always does; a Standalone fixture only
	// does when it modelled the cache-hydration path.
	hasNodeContent bool

	// v6 and v6Sites are the projection-v6 dialect's tree and its call-site
	// nodes (one per ref, in b.refs order), built once on first use. Unused by
	// the v4 dialects.
	v6      *v6Tree
	v6Sites []*v6Node
}

// insertRows writes every spec inside ONE transaction: a fixture is
// all-or-nothing, and a benchmark-sized one (thousands of `_ast` rows) costs
// a single fsync rather than one per row.
func (b *Builder) insertRows(db *sql.DB) {
	b.t.Helper()
	tx, err := db.Begin()
	if err != nil {
		b.t.Fatalf("fixturedb(%s): begin: %v", b.producer, err)
	}
	d := b.producer.dialect
	e := &emitter{
		b: b, db: tx,
		hasNodeContent: d.hasNodeContent(b),
		derivedParent:  sqlintro.ColumnIsGenerated(tx, "nodes", "parent_id"),
	}
	d.emitNodes(e)
	d.emitSymbols(e)
	d.emitAST(e)
	d.emitSources(e)
	d.emitImports(e)
	e.emitLSP()
	if err := tx.Commit(); err != nil {
		b.t.Fatalf("fixturedb(%s): commit: %v", b.producer, err)
	}
}

func (e *emitter) exec(q string, args ...any) {
	e.b.t.Helper()
	if _, err := e.db.Exec(q, args...); err != nil {
		e.b.t.Fatalf("fixturedb(%s): %v\n%s", e.b.producer, err, q)
	}
}

// subtree records a deduped merkle subtree in node_content and returns the hash
// occurrences point at. Returns nil when the fixture has no node_content table,
// so node_hash lands NULL rather than dangling.
func (e *emitter) subtree(label, kind, token string) []byte {
	if !e.hasNodeContent {
		return nil
	}
	h := subtreeHash(label)
	e.exec(`INSERT OR IGNORE INTO node_content (node_hash, node_tag, kind, raw_kind, lang, token, arity)
		VALUES (?, 0, ?, ?, '', ?, 0)`, h, kind, kind, nullIfEmpty(token))
	return h
}

// insertV4Nodes writes one projection-v4 `nodes` row per construct.
func (e *emitter) insertV4Nodes() {
	for _, id := range e.b.order {
		c := e.b.constructs[id]
		kind := 1
		if c.leaf {
			kind = 0
		}
		e.insertV4Node(string(c.id), string(c.parent), c.name, kind, c.record, string(c.source))
	}
}

// insertV4Node writes one projection-v4 `nodes` row, leaving parent_id to
// SQLite when the table generates it.
func (e *emitter) insertV4Node(id, parent, name string, kind int, record any, source string) {
	if e.derivedParent {
		e.exec(`INSERT OR REPLACE INTO nodes (id, name, kind, size, mtime, record_id, record, source_file)
			VALUES (?, ?, ?, 0, 0, '', ?, ?)`, id, name, kind, record, source)
		return
	}
	e.exec(`INSERT OR REPLACE INTO nodes (id, parent_id, name, kind, size, mtime, record_id, record, source_file)
		VALUES (?, ?, ?, ?, 0, 0, '', ?, ?)`, id, parent, name, kind, record, source)
}

// insertV4AST writes the projection-v4 `_ast` rows and returns each node's
// hash, keyed by node id, for the child lists.
func (e *emitter) insertV4AST() map[string][]byte {
	hashes := make(map[string][]byte, len(e.b.ast))
	for _, a := range e.b.ast {
		h := e.subtree(a.subtree, a.kind, a.token)
		hashes[a.nodeID] = h
		e.exec(`INSERT OR REPLACE INTO _ast
			(node_id, source_id, node_kind, start_byte, end_byte, start_row, start_col, end_row, end_col, node_hash)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			a.nodeID, string(a.source), a.kind,
			a.span.StartByte, a.span.EndByte,
			a.span.StartRow, a.span.StartCol, a.span.EndRow, a.span.EndCol, h)
	}
	return hashes
}

// insertV4Sources writes the projection-v4 `_source` rows.
func (e *emitter) insertV4Sources() {
	for _, id := range e.b.srcOrder {
		s := e.b.sources[id]
		var body any
		if s.content != "" {
			body = []byte(s.content)
		}
		e.exec(`INSERT OR REPLACE INTO _source (id, language, content, path, content_hash)
			VALUES (?, ?, ?, ?, NULL)`, string(s.id), s.lang, body, s.path)
	}
}

// insertV6AST writes the projection-v6 `_ast` rows, keyed by nid, and returns
// each node's hash, keyed by node id, for the child lists.
func (e *emitter) insertV6AST() map[string][]byte {
	t := e.v6Tree()
	hashes := make(map[string][]byte, len(e.b.ast))
	for _, a := range e.b.ast {
		h := e.subtree(a.subtree, a.kind, a.token)
		hashes[a.nodeID] = h
		e.exec(`INSERT OR REPLACE INTO _ast
			(nid, kind_id, start_byte, end_byte, start_row, start_col, end_row, end_col, node_hash)
			VALUES (?, (SELECT kind_id FROM kinds WHERE raw_kind = ? AND lang = ?), ?, ?, ?, ?, ?, ?, ?)`,
			t.byPath[a.nodeID].nid, a.kind, e.b.langOf(a.source),
			a.span.StartByte, a.span.EndByte, a.span.StartRow, a.span.StartCol, a.span.EndRow, a.span.EndCol, h)
	}
	return hashes
}

// insertV6Sources writes the projection-v6 `_source` rows, each with the
// interned file id its nodes are keyed under.
func (e *emitter) insertV6Sources() {
	t := e.v6Tree()
	for _, id := range e.b.srcOrder {
		s := e.b.sources[id]
		var body any
		if s.content != "" {
			body = []byte(s.content)
		}
		e.exec(`INSERT OR REPLACE INTO _source (id, language, content, path, content_hash, file_id)
			VALUES (?, ?, ?, ?, NULL, ?)`, string(s.id), s.lang, body, s.path, t.byPath[string(s.id)].fileID)
	}
}

// childRow is one `node_child` row as the fixture states it: the child's hash
// and the field it sits under.
type childRow struct {
	hash  []byte
	field string
}

// emitNodeChildren writes the merkle child list: for every `_ast` node whose
// id's directory is itself an `_ast` node, one row under the parent's hash at
// the child's start-byte position among its siblings. ley-line emits a parent's
// list once per unique hash, so two ASTNodes sharing a [Detail.Subtree] label
// share one list — and must therefore agree on it: identical content has
// identical children, so a fixture whose labelled twins disagree is describing
// a subtree that cannot exist.
func (e *emitter) emitNodeChildren(hashes map[string][]byte) {
	byParent := make(map[string][]astSpec)
	for _, a := range e.b.ast {
		parent := path.Dir(a.nodeID)
		if _, ok := hashes[parent]; ok {
			byParent[parent] = append(byParent[parent], a)
		}
	}
	listed := make(map[string][]childRow)
	for _, a := range e.b.ast {
		kids, ok := byParent[a.nodeID]
		if !ok {
			continue
		}
		slices.SortStableFunc(kids, func(x, y astSpec) int { return x.span.StartByte - y.span.StartByte })
		rows := make([]childRow, len(kids))
		for i, k := range kids {
			rows[i] = childRow{hash: hashes[k.nodeID], field: k.field}
		}
		key := hex.EncodeToString(hashes[a.nodeID])
		if prior, seen := listed[key]; seen {
			if !sameChildList(prior, rows) {
				e.b.t.Fatalf("fixturedb: %s shares a Subtree label with an earlier ASTNode "+
					"but declares different children — one subtree has one child list", a.nodeID)
			}
			continue
		}
		listed[key] = rows
		for ord, r := range rows {
			e.exec(`INSERT INTO node_child (parent_hash, ordinal, child_hash, field) VALUES (?, ?, ?, ?)`,
				hashes[a.nodeID], ord, r.hash, nullIfEmpty(r.field))
		}
	}
}

// sameChildList reports whether two child lists are the same list: same
// hashes in the same order under the same fields.
func sameChildList(a, b []childRow) bool {
	return slices.EqualFunc(a, b, func(x, y childRow) bool {
		return slices.Equal(x.hash, y.hash) && x.field == y.field
	})
}

// emitLSP writes the LSP-enrichment rows: _lsp_defs, _lsp and _lsp_hover.
func (e *emitter) emitLSP() {
	_, v6 := e.b.producer.dialect.(leylineV6Dialect)
	// key is the column value naming a node: its nid on v6, which the spec
	// must declare, its id on v4.
	key := func(table, id string) any {
		if !v6 {
			return id
		}
		n, ok := e.v6Tree().byPath[id]
		if !ok {
			e.b.t.Fatalf("fixturedb(leyline-v6): %s row on %q, which the spec never declares; "+
				"v6 keys %s by the node's nid, so the node must exist", table, id, table)
		}
		return n.nid
	}
	col := "node_id"
	if v6 {
		col = "nid"
	}
	for _, d := range e.b.lspDefs {
		e.exec(`INSERT INTO _lsp_defs
			(`+col+`, def_token, def_uri, def_start_line, def_start_col, def_end_line, def_end_col)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			key("_lsp_defs", d.nodeID), d.token, d.uri, d.startLine, d.startCol, d.endLine, d.endCol)
	}
	for _, l := range e.b.lspSyms {
		e.exec(`INSERT INTO _lsp (`+col+`, symbol_kind, detail, start_line, start_col, end_line, end_col, diagnostics)
			VALUES (?, ?, '', 0, 0, 0, 0, ?)`, key("_lsp", l.nodeID), l.symbolKind, nullIfEmpty(l.diagnostics))
	}
	for _, h := range e.b.lspHovers {
		if !v6 {
			e.insertV4Chain(h.symbol)
		}
		e.exec(`INSERT OR REPLACE INTO _lsp_hover (`+col+`, hover_text) VALUES (?, ?)`, key("_lsp_hover", h.symbol), h.text)
	}
}

// insertV4Chain writes a standalone address's nodes rows on projection-v4, one
// directory per link, as LLO's standalone_chain does. v6 interns the same chain
// as directories when it builds the tree.
func (e *emitter) insertV4Chain(p string) {
	parent := ""
	for _, seg := range strings.Split(p, "/") {
		id := seg
		if parent != "" {
			id = parent + "/" + seg
		}
		e.insertV4Node(id, parent, seg, 1, "", "")
		parent = id
	}
}

// sourceOf resolves a construct's source file, falling back to inference so
// Def/Ref never depend on declaration order.
func (b *Builder) sourceOf(id ConstructID) SourceID {
	if c, ok := b.constructs[id]; ok {
		return c.source
	}
	return inferSource(id)
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
