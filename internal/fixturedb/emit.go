package fixturedb

import "database/sql"

// Emission: the ONE place a spec becomes columns.
//
// Every producer difference in this package lives here, in a `switch
// b.producer` — never in a caller, never in a `CREATE TABLE` a test wrote. That
// is what makes "which producer did this test mean?" a question with an answer.

// emitter carries the per-build state the row writers share.
type emitter struct {
	b  *Builder
	db *sql.DB
	// hasNodeContent reports whether this fixture has a node_content table to
	// point node_hash values at. Ley-line always does; a Standalone fixture only
	// does when it modelled the cache-hydration path.
	hasNodeContent bool
}

func (b *Builder) insertRows(db *sql.DB) {
	b.t.Helper()
	e := &emitter{b: b, db: db, hasNodeContent: b.producer == Leyline || len(b.ast) > 0}
	e.emitNodes()
	e.emitDefs()
	e.emitRefs()
	e.emitAST()
	e.emitSources()
	e.emitImports()
	e.emitLSPDefs()
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

func (e *emitter) emitNodes() {
	for _, id := range e.b.order {
		c := e.b.constructs[id]
		kind := 1
		if c.dirKind {
			kind = 0
		}
		e.exec(`INSERT OR REPLACE INTO nodes (id, parent_id, name, kind, size, mtime, record_id, record, source_file)
			VALUES (?, ?, ?, ?, 0, 0, '', '', ?)`,
			string(c.id), string(c.parent), c.name, kind, string(c.source))
	}
}

func (e *emitter) emitDefs() {
	for _, d := range e.b.defs {
		if e.b.producer != Leyline {
			// The mache projection carries two columns. Everything the spec
			// says about kind, container and content identity is DROPPED —
			// which is the honest outcome, not a lossy shortcut.
			e.exec(`INSERT OR IGNORE INTO node_defs (token, node_id) VALUES (?, ?)`,
				d.token, string(d.nodeID))
			continue
		}
		h := e.subtree(d.subtree, string(d.kind), d.token)
		e.exec(`INSERT INTO node_defs (token, node_id, source_id, container_node_id, canonical_kind, node_hash)
			VALUES (?, ?, ?, ?, ?, ?)`,
			d.token, string(d.nodeID), string(e.b.sourceOf(d.nodeID)),
			nullIfEmpty(string(d.container)), nullIfEmpty(string(d.kind)), h)
	}
}

func (e *emitter) emitRefs() {
	for _, r := range e.b.refs {
		if e.b.producer != Leyline {
			// No site column and no qualifier column: the ENCLOSING CONSTRUCT
			// is what lands in node_id, and duplicate (token, node_id) pairs
			// collapse under the primary key.
			e.exec(`INSERT OR IGNORE INTO node_refs (token, node_id) VALUES (?, ?)`,
				r.token, string(r.from))
			continue
		}
		h := e.subtree(r.subtree, "call_expression", r.token)
		e.exec(`INSERT INTO node_refs (token, node_id, source_id, container_node_id, qualifier, node_hash)
			VALUES (?, ?, ?, ?, ?, ?)`,
			r.token, string(r.at), string(e.b.sourceOf(r.from)), string(r.from),
			nullIfEmpty(r.qualifier), h)
	}
}

func (e *emitter) emitAST() {
	for _, a := range e.b.ast {
		h := e.subtree(a.subtree, a.kind, a.token)
		e.exec(`INSERT OR REPLACE INTO _ast
			(node_id, source_id, node_kind, start_byte, end_byte, start_row, start_col, end_row, end_col, node_hash)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			a.nodeID, string(a.source), a.kind,
			a.span.StartByte, a.span.EndByte,
			a.span.StartRow, a.span.StartCol, a.span.EndRow, a.span.EndCol, h)
	}
}

func (e *emitter) emitSources() {
	if e.b.producer != Leyline && len(e.b.sources) == 0 {
		return
	}
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

func (e *emitter) emitImports() {
	if e.b.producer != Leyline {
		return // the mache projection has no _imports table
	}
	for _, im := range e.b.imports {
		e.exec(`INSERT INTO _imports (alias, path, source_id) VALUES (?, ?, ?)`,
			im.alias, im.importPath, string(im.source))
	}
}

func (e *emitter) emitLSPDefs() {
	for _, d := range e.b.lspDefs {
		e.exec(`INSERT INTO _lsp_defs
			(node_id, def_token, def_uri, def_start_line, def_start_col, def_end_line, def_end_col)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			d.nodeID, d.token, d.uri, d.startLine, d.startCol, d.endLine, d.endCol)
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
