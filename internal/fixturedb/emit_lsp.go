package fixturedb

import "strings"

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
