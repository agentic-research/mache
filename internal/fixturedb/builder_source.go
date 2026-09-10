package fixturedb

// The producer-side declarations: what ley-line's parse writes about a source
// file (`_source`, `_ast` + `nodes`, `node_child`, `_imports`) and what LSP
// enrichment adds (`_lsp_defs`). The mache-projection side of the fixture —
// constructs, defs, refs — is builder.go.

// ASTNode declares one `_ast` row: a parse-tree node of tree-sitter kind `kind`
// spanning `span` in source `in`.
//
// It also declares the node's `nodes` row, because ley-line writes one for
// every parse-tree node it keeps and the projection's file index JOINs the two
// — an `_ast` row without its `nodes` row is a node the walker cannot see. A
// leaf's token lands in `nodes.record` too, as ley-line writes it.
//
// On [Leyline] the node's position under its parent goes to `node_child`: the
// parent is the ASTNode whose id is this id's directory, children are ordered
// by start byte, and [Detail.Field] names the field. Every intermediate node a
// selector walks through must therefore be declared — as in a real parse.
func (b *Builder) ASTNode(id, kind string, in SourceID, span Span, detail ...Detail) *Builder {
	d := first(detail)
	b.ast = append(b.ast, astSpec{
		nodeID: id, kind: kind, source: in, span: span,
		token: d.Token, subtree: d.label(b.nextSubtree()), field: d.Field,
	})
	b.Construct(ConstructID(id), Where{Source: in})
	if d.Token != "" {
		b.constructs[ConstructID(id)].record = d.Token
	}
	b.Source(in, "", "")
	return b
}

// Source declares a source file. lang may be empty when the test does not care;
// content may be empty, in which case `_source.content` is NULL — which is what
// ley-line v0.13.0 writes (it stores bytes in source_blobs and only a path
// here). Pass content when the test is about snippet extraction.
//
// Re-declaring a source fills in whichever of lang/content was previously empty
// rather than replacing it, so ASTNode's implicit declaration never clobbers an
// explicit one.
func (b *Builder) Source(id SourceID, lang, content string) *Builder {
	s, ok := b.sources[id]
	if !ok {
		s = &sourceSpec{id: id, path: "/synthetic/" + string(id)}
		b.sources[id] = s
		b.srcOrder = append(b.srcOrder, id)
	}
	if lang != "" {
		s.lang = lang
	}
	if content != "" {
		s.content = content
	}
	return b
}

// SourceFile declares a source file by PATH — `_source.content` NULL and
// `_source.path` pointing at a real file, which is what ley-line writes by
// default and what the projection reads from disk when the row carries no
// bytes. Re-declaring a source this way overrides the synthetic path a prior
// [Builder.Source] or [Builder.ASTNode] gave it, so the two can be stated in
// either order.
func (b *Builder) SourceFile(id SourceID, lang, path string) *Builder {
	b.Source(id, lang, "")
	b.sources[id].path = path
	return b
}

// Import declares one `_imports` row: `alias` bound to module `importPath`
// inside source `in`. Only [Leyline] has this table; on [Standalone] the call is
// dropped.
func (b *Builder) Import(alias, importPath string, in SourceID) *Builder {
	b.imports = append(b.imports, importSpec{alias: alias, importPath: importPath, source: in})
	b.Source(in, "", "")
	return b
}

// LSPDef declares one `_lsp_defs` row — a binding-fidelity definition, which
// ensureCanonicalViews unions into v_defs when `_lsp_defs.def_token` exists.
// The table is LSP-enrichment output, not a producer table, so it is available
// on both producers.
func (b *Builder) LSPDef(token string, in ConstructID, uri string, startLine, startCol, endLine, endCol int) *Builder {
	b.lspDefs = append(b.lspDefs, lspDefSpec{
		nodeID: string(in), token: token, uri: uri,
		startLine: startLine, startCol: startCol, endLine: endLine, endCol: endCol,
	})
	return b
}
