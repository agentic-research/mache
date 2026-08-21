package fixturedb

import (
	"crypto/sha256"
	"path"
	"strconv"
	"strings"
	"testing"

	_ "modernc.org/sqlite" // pure-Go driver; fixtures must not need CGO
)

// Builder accumulates a fixture's CONTENT. It has no method that accepts SQL,
// DDL, a column name or a table name — that absence is the design, not an
// oversight, and adding such a method re-opens the defect this package closed.
//
// Every method returns the receiver so calls chain.
type Builder struct {
	t        *testing.T
	producer Producer

	constructs map[ConstructID]*construct
	order      []ConstructID
	defs       []defSpec
	refs       []refSpec
	ast        []astSpec
	sources    map[SourceID]*sourceSpec
	srcOrder   []SourceID
	imports    []importSpec
	lspDefs    []lspDefSpec

	siteSeq map[ConstructID]int
	subtree int
}

type lspDefSpec struct {
	nodeID, token, uri  string
	startLine, startCol int
	endLine, endCol     int
}

// New starts a fixture for the given producer. p is MANDATORY and must be one
// of [Leyline] / [Standalone]; the zero Producer fails the test immediately,
// which is how "a fixture cannot exist without naming its producer" is enforced
// at the one place a fixture can be created.
func New(t *testing.T, p Producer) *Builder {
	t.Helper()
	if !p.valid() {
		t.Fatal("fixturedb.New: producer is required — pass fixturedb.Leyline or fixturedb.Standalone")
	}
	return &Builder{
		t:          t,
		producer:   p,
		constructs: map[ConstructID]*construct{},
		sources:    map[SourceID]*sourceSpec{},
		siteSeq:    map[ConstructID]int{},
	}
}

// Producer reports which producer shape this fixture reproduces, so a test can
// assert on it or branch its expectations explicitly rather than by accident.
func (b *Builder) Producer() Producer { return b.producer }

// Construct declares an enclosing definition and its `nodes` row. Declaring the
// same id twice fills in whatever [Where] adds rather than duplicating the
// construct, so a fixture can name a construct before it knows its source.
func (b *Builder) Construct(id ConstructID, where ...Where) *Builder {
	c, ok := b.constructs[id]
	if !ok {
		c = &construct{id: id, source: inferSource(id), name: path.Base(string(id))}
		b.constructs[id] = c
		b.order = append(b.order, id)
	}
	w := first(where)
	if w.Source != "" {
		c.source = w.Source
	}
	if w.Name != "" {
		requireNameMatchesID(b.t, id, w.Name)
		c.name = w.Name
	}
	if w.Parent != "" {
		requireParentMatchesID(b.t, id, w.Parent, c.name)
		c.parent = w.Parent
	}
	if w.Directory {
		c.dirKind = true
	}
	return b
}

// Def declares that `token` is DEFINED by construct `in`, with κ kind `kind`.
//
// On [Leyline] this writes token / node_id / source_id / container_node_id /
// canonical_kind / node_hash. On [Standalone] it writes token and node_id and
// DROPS the rest, because the mache projection has nowhere to put them — which
// is the point: the same test source now produces the shape it claims.
func (b *Builder) Def(token string, in ConstructID, kind CanonicalKind, detail ...Detail) *Builder {
	b.Construct(in)
	d := first(detail)
	spec := defSpec{
		token: token, nodeID: in, kind: kind,
		container: d.Container, subtree: d.label(b.nextSubtree()),
	}
	if spec.container != "" {
		b.Construct(spec.container)
	}
	b.defs = append(b.defs, spec)
	return b
}

// Ref declares that `token` is REFERENCED from inside construct `from`, at
// occurrence `at`, through selector `qualifier`.
//
// `from` and `at` are separate parameters because `node_refs.node_id` means two
// incompatible things:
//
//   - [Leyline] writes node_id = at (the call-SITE leaf), container_node_id =
//     from (the enclosing def), plus source_id / qualifier / node_hash.
//   - [Standalone] writes node_id = from (the enclosing CONSTRUCT) and has no
//     column for at or qualifier at all.
//
// An empty `at` means "an unnamed occurrence": the builder synthesises a unique
// ley-line-shaped call-site leaf under `from`. An empty `qualifier` is stored as
// NULL, which is what ley-line writes for a bare (unqualified) call.
func (b *Builder) Ref(token string, from ConstructID, at SiteID, qualifier string, detail ...Detail) *Builder {
	b.Construct(from)
	d := first(detail)
	if at == "" {
		at = b.nextSite(from)
	}
	b.refs = append(b.refs, refSpec{
		token: token, from: from, at: at, qualifier: qualifier,
		subtree: d.label(b.nextSubtree()),
	})
	return b
}

// nextSite synthesises a ley-line-shaped call-site leaf under from, so two
// unnamed occurrences in one construct stay two distinct rows.
func (b *Builder) nextSite(from ConstructID) SiteID {
	n := b.siteSeq[from]
	b.siteSeq[from] = n + 1
	return SiteID(string(from) +
		"/block/statement_list/expression_statement/call_expression_" + strconv.Itoa(n))
}

// ASTNode declares one `_ast` row: a parse-tree node of tree-sitter kind `kind`
// spanning `span` in source `in`.
func (b *Builder) ASTNode(id, kind string, in SourceID, span Span, detail ...Detail) *Builder {
	d := first(detail)
	b.ast = append(b.ast, astSpec{
		nodeID: id, kind: kind, source: in, span: span,
		token: d.Token, subtree: d.label(b.nextSubtree()),
	})
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

func (b *Builder) nextSubtree() string {
	b.subtree++
	return "seq:" + strconv.Itoa(b.subtree)
}

// inferSource returns the longest leading path prefix of id whose final segment
// looks like a file name (contains a '.'), which is how ley-line composes node
// ids: "<source_id>/<tree-sitter path>".
func inferSource(id ConstructID) SourceID {
	segs := strings.Split(string(id), "/")
	for i := len(segs) - 1; i >= 0; i-- {
		if strings.Contains(segs[i], ".") {
			return SourceID(strings.Join(segs[:i+1], "/"))
		}
	}
	return ""
}

// subtreeHash maps a subtree LABEL to a stable 32-byte merkle-shaped hash. Two
// occurrences with the same label get the same hash; different labels
// (including the per-occurrence sequence labels) get different ones.
func subtreeHash(label string) []byte {
	sum := sha256.Sum256([]byte(label))
	return sum[:]
}

// requireNameMatchesID refuses a name that is not the id's last path segment.
//
// ley-line ALWAYS writes nodes.name as the final segment of nodes.id — verified
// against a v0.19.0 arena, where `a.go/package_clause` has name
// `package_clause`, not a symbol. A fixture that sets a symbol here is stating a
// shape the producer never emits, which is the class of hidden test parameter
// this package exists to remove.
//
// It became load-bearing in projection-v4: parent_id is now DERIVED by stripping
// the trailing "/"+name from the id, so an inconsistent name silently yields a
// garbage parent — and six smell rules join on parent_id. Before v4 the stored
// column hid it. A symbol belongs on Def(token, ...), which every such call site
// already makes.
func requireNameMatchesID(t *testing.T, id ConstructID, name string) {
	t.Helper()
	if want, ok := nameMatchesID(id, name); !ok {
		t.Fatalf("fixturedb: Where{Name: %q} on construct %q — ley-line always writes "+
			"nodes.name as the id's last segment (%q here), and projection-v4 derives "+
			"parent_id by stripping \"/\"+name from the id, so this would give the row a "+
			"parent nobody wrote. Drop Where{Name} (a symbol belongs on Def), or use an "+
			"id whose last segment IS the name.", name, id, want)
	}
}

// requireParentMatchesID refuses a parent that the id does not spell, for the
// same reason: under a derived parent_id the stored value is not consulted, so a
// fixture whose Parent disagrees with its id is describing a node that cannot
// exist.
func requireParentMatchesID(t *testing.T, id, parent ConstructID, name string) {
	t.Helper()
	if want := string(parent) + "/" + name; string(id) != want {
		t.Fatalf("fixturedb: Where{Parent: %q} on construct %q with name %q — the id must "+
			"be parent+\"/\"+name (%q), because projection-v4 DERIVES parent_id from the id "+
			"and would ignore this value.", parent, id, name, want)
	}
}

// nameMatchesID reports whether name is the id's last path segment, returning
// the segment it should have been. Separated from the reporting so the rule can
// be asserted directly, without a *testing.T whose Fatalf would terminate the
// test making the assertion.
func nameMatchesID(id ConstructID, name string) (string, bool) {
	want := path.Base(string(id))
	return want, name == want
}

// parentMatchesID reports whether the id is exactly parent+"/"+name, returning
// the id it should have been.
func parentMatchesID(id, parent ConstructID, name string) (string, bool) {
	want := string(parent) + "/" + name
	return want, string(id) == want
}
