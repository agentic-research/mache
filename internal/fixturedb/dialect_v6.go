package fixturedb

import (
	"maps"
	"slices"
)

// leylineV6Dialect models ley-line-open's projection-v6 (v0.20.x): integer
// nids, interned names and kinds, and paths derived by LLO's own views rather
// than stored. See v6tree.go for the layout.
//
// The same spec that builds a Leyline (v4) fixture builds this one, and every
// declared construct's v_node_path is its v4 id. That is what lets a reader be
// tested for the same answers from both shapes. The cost is that a spec must
// name syntax nodes the way leyline does: a kind is suffixed "_k" only when it
// repeats among its siblings, and a spec that says otherwise is refused.
type leylineV6Dialect struct{}

func (leylineV6Dialect) schema(b *Builder) []string {
	stmts := tableDDL(leylineV6TableOrder, leylineV6Tables)
	stmts = append(stmts, b.lspDDL(true)...)
	stmts = append(stmts, tableDDL(slices.Sorted(maps.Keys(leylineV6Indexes)), leylineV6Indexes)...)
	// v_node_path reads v_node_name, so name first.
	return append(stmts, leylineV6Views["v_node_name"], leylineV6Views["v_node_path"])
}

// leylineV6TableOrder is creation order: interning tables first, then the
// tables that reference them.
var leylineV6TableOrder = []string{
	"names", "kinds", "dirs", "files", "nodes", "node_content", "node_child",
	"_ast", "_source", "node_defs", "node_refs", "_imports",
}

// Ley-line always writes node_content.
func (leylineV6Dialect) hasNodeContent(*Builder) bool { return true }

func (leylineV6Dialect) emitNodes(e *emitter) {
	t := e.v6Tree()
	names := newInterner(e, `INSERT INTO names (name_id, text) VALUES (?, ?)`)
	kinds := map[[2]string]int{}
	kindID := func(lang, raw string) any {
		if raw == "" {
			return nil
		}
		key := [2]string{lang, raw}
		if id, ok := kinds[key]; ok {
			return id
		}
		id := len(kinds) + 1
		kinds[key] = id
		e.exec(`INSERT INTO kinds (kind_id, lang, raw_kind) VALUES (?, ?, ?)`, id, lang, raw)
		return id
	}

	var walk func(n *v6Node, lang string)
	walk = func(n *v6Node, lang string) {
		var nameID any
		if n.isDir || n.isFile {
			nameID = names.id(nameText(n))
		}
		if n.isFile {
			lang = e.b.langOf(SourceID(n.path))
		}
		var kind any
		if !n.isDir {
			kind = kindID(lang, n.kind)
		}
		var parent any
		if n.parent != nil {
			parent = n.parent.nid
		}
		e.exec(`INSERT INTO nodes (nid, parent_nid, name_id, kind_id, kind, ord, size, mtime, record_id, record, source_file)
			VALUES (?, ?, ?, ?, ?, ?, 0, 0, NULL, ?, NULL)`,
			n.nid, parent, nameID, kind, e.v6NodeKind(n), n.ord, e.v6Record(n))
		switch {
		case n == t.root:
			e.exec(`INSERT INTO dirs (dir_id, parent_dir_id, name_id) VALUES (1, NULL, ?)`, nameID)
		case n.isDir:
			e.exec(`INSERT INTO dirs (dir_id, parent_dir_id, name_id) VALUES (?, ?, ?)`, n.dirID, n.parent.dirID, nameID)
		case n.isFile:
			e.exec(`INSERT INTO files (file_id, dir_id, name_id) VALUES (?, ?, ?)`, n.fileID, n.parent.dirID, nameID)
		}
		for _, c := range n.kids {
			walk(c, lang)
		}
	}
	walk(t.root, "")
}

// v6NodeKind is nodes.kind. A declared construct keeps the value the v4
// fixture gives it, so a reader sees the same node kind from either shape.
// A node the spec only implies gets what leyline writes: 1 when it has
// children (every directory and file does), 0 for a leaf.
func (e *emitter) v6NodeKind(n *v6Node) int {
	if c, ok := e.b.constructs[ConstructID(n.path)]; ok && n.path != "" {
		if c.leaf {
			return 0
		}
		return 1
	}
	if n.isDir || n.isFile || len(n.kids) > 0 {
		return 1
	}
	return 0
}

// v6Record is nodes.record: a declared leaf's token, as ley-line writes it.
func (e *emitter) v6Record(n *v6Node) any {
	if c, ok := e.b.constructs[ConstructID(n.path)]; ok && c.record != "" {
		return c.record
	}
	return nil
}

func (leylineV6Dialect) emitSymbols(e *emitter) {
	t := e.v6Tree()
	for _, d := range e.b.defs {
		n := t.byPath[string(d.nodeID)]
		var container any
		if d.container != "" {
			container = t.byPath[string(d.container)].nid
		}
		h := e.subtree(d.subtree, string(d.kind), d.token)
		sp := e.b.spanOf(string(d.nodeID))
		e.exec(`INSERT INTO node_defs (token, nid, container_nid, node_kind,
			start_byte, end_byte, start_row, start_col, end_row, end_col, canonical_kind, node_hash)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			d.token, n.nid, container, nullIfEmpty(n.kind),
			sp.startByte, sp.endByte, sp.startRow, sp.startCol, sp.endRow, sp.endCol,
			nullIfEmpty(string(d.kind)), h)
	}
	for i, r := range e.b.refs {
		site := e.v6Sites[i]
		h := e.subtree(r.subtree, "call_expression", r.token)
		e.exec(`INSERT INTO node_refs (token, nid, container_nid, node_kind, qualifier, node_hash)
			VALUES (?, ?, ?, ?, ?, ?)`,
			r.token, site.nid, t.byPath[string(r.from)].nid, nullIfEmpty(site.kind),
			nullIfEmpty(r.qualifier), h)
	}
}

func (leylineV6Dialect) emitAST(e *emitter) { e.emitNodeChildren(e.insertV6AST()) }

func (leylineV6Dialect) emitSources(e *emitter) { e.insertV6Sources() }

func (leylineV6Dialect) emitImports(e *emitter) { leylineDialect{}.emitImports(e) }
