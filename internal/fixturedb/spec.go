package fixturedb

// The row specs a [Builder] accumulates. They are the fixture's CONTENT —
// what is true about the code being modelled — and carry no shape: which
// columns each becomes is decided per producer at emit time (emit.go).

type construct struct {
	id     ConstructID
	source SourceID
	name   string
	parent ConstructID
	leaf   bool
	// record is `nodes.record`: the token text of a leaf `_ast` node, which
	// ley-line writes there as well as into node_content. Empty otherwise.
	record string
}

type defSpec struct {
	token     string
	nodeID    ConstructID
	container ConstructID
	kind      CanonicalKind
	subtree   string
}

type refSpec struct {
	token     string
	from      ConstructID
	at        SiteID
	qualifier string
	subtree   string
	// synthetic is true when the spec left the site unnamed and the builder
	// made one up. A v6 fixture gives such a site its own node and lets the
	// naming rule name it, rather than trusting the made-up path.
	synthetic bool
}

type astSpec struct {
	nodeID  string
	kind    string
	source  SourceID
	span    Span
	token   string
	subtree string
	field   string
}

type sourceSpec struct {
	id      SourceID
	lang    string
	content string
	path    string
}

type importSpec struct {
	alias, importPath string
	source            SourceID
}

type lspDefSpec struct {
	nodeID, token, uri  string
	startLine, startCol int
	endLine, endCol     int
}

// lspHoverSpec is one _lsp_hover row. symbol is the standalone address
// `leyline lsp` keys hover by: "symbols/" plus the document-symbol path.
type lspHoverSpec struct{ symbol, text string }

// lspSymbolSpec is one _lsp row: a document symbol merged onto the AST node its
// name is in, with the JSON diagnostics in its range ("" for none).
type lspSymbolSpec struct{ nodeID, symbolKind, diagnostics string }
