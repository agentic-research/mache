package fixturedb

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWhere_ShapesTheNodesRow covers every field of [Where].
// They matter because enrichLocations and several rules join back through
// `nodes`, so a fixture that gets parent_id or source_file wrong changes what
// the rule under test reports without changing what it queries.
func TestWhere_ShapesTheNodesRow(t *testing.T) {
	b := New(t, Leyline)
	b.Construct("pkg", Where{Directory: true})
	// id, name and parent must agree: ley-line writes nodes.name as the id's
	// last segment, and projection-v4 DERIVES parent_id by stripping
	// "/"+name from the id. A fixture that disagrees describes a node the
	// producer cannot emit — see TestWhere_RefusesAnIncoherentNodeShape.
	b.Construct("pkg/Orphan", Where{Parent: "pkg", Name: "Orphan", Source: "pkg/orphan.go"})
	_, f := b.Build()

	var parent, name string
	var kind int
	var source string
	require.NoError(t, f.DB().QueryRow(
		`SELECT parent_id, name, kind, source_file FROM nodes WHERE id=?`,
		"pkg/Orphan").Scan(&parent, &name, &kind, &source))
	assert.Equal(t, "pkg", parent)
	assert.Equal(t, "Orphan", name)
	assert.Equal(t, 1, kind, "a construct is a leaf node")
	assert.Equal(t, "pkg/orphan.go", source)

	require.NoError(t, f.DB().QueryRow(
		`SELECT kind FROM nodes WHERE id='pkg'`).Scan(&kind))
	assert.Equal(t, 0, kind, "Where{Directory:true} marks a directory node")
}

// TestWhere_RefusesAnIncoherentNodeShape pins the guard that projection-v4 made
// load-bearing.
//
// Before v4, nodes.parent_id was STORED, so a fixture could set a name unrelated
// to its id and the written parent still won. v4 derives parent_id by stripping
// "/"+name from the id, so the same fixture now yields a parent nobody wrote —
// and six smell rules join on parent_id. The failure is invisible: a wrong
// parent reads as an empty directory, not as an error.
//
// Asserted on the predicates rather than through Construct, whose Fatalf would
// terminate the test making the assertion.
func TestWhere_RefusesAnIncoherentNodeShape(t *testing.T) {
	t.Run("name must be the id's last segment", func(t *testing.T) {
		// The real shape: ley-line writes `a.go/package_clause` with name
		// `package_clause`, never with a symbol.
		want, ok := nameMatchesID("a.go/package_clause", "package_clause")
		assert.True(t, ok)
		assert.Equal(t, "package_clause", want)

		// The shape three tests used to state: a SYMBOL as the name of a
		// grammar-path node. That belongs on Def(token, ...).
		want, ok = nameMatchesID("src/lib.rs/impl_item_0/declaration_list/function_item_0", "new")
		assert.False(t, ok, "a symbol is not the id's last segment")
		assert.Equal(t, "function_item_0", want, "the guard must name the segment it expected")
	})

	t.Run("id must be parent + / + name", func(t *testing.T) {
		want, ok := parentMatchesID("pkg/Orphan", "pkg", "Orphan")
		assert.True(t, ok)
		assert.Equal(t, "pkg/Orphan", want)

		want, ok = parentMatchesID("pkg/orphan.go/function_declaration_0", "pkg", "Orphan")
		assert.False(t, ok, "the id does not spell this parent")
		assert.Equal(t, "pkg/Orphan", want)
	})
}

// TestDetailContainer_PopulatesTheDefContainer covers node_defs.container_node_id,
// which ley-line populates for a method inside an impl/type block and leaves
// NULL for a top-level item. The self-described "Leyline column shape" fixture
// this package replaced omitted the column entirely (mache-e3f3bf).
func TestDetailContainer_PopulatesTheDefContainer(t *testing.T) {
	b := New(t, Leyline)
	b.Def("Handle", "src/lib.rs/impl_item_0/function_item_0", Method,
		Detail{Container: "src/lib.rs/impl_item_0"})
	b.Def("free", "src/lib.rs/function_item_1", Function)
	_, f := b.Build()

	var container sql.NullString
	require.NoError(t, f.DB().QueryRow(
		`SELECT container_node_id FROM node_defs WHERE token='Handle'`).Scan(&container))
	assert.Equal(t, "src/lib.rs/impl_item_0", container.String)

	require.NoError(t, f.DB().QueryRow(
		`SELECT container_node_id FROM node_defs WHERE token='free'`).Scan(&container))
	assert.False(t, container.Valid, "ley-line leaves a top-level item's container NULL")
}

// TestDetailSubtree_OnRefs mirrors the def-side test: two call sites that are
// the same deduped subtree share one node_hash.
func TestDetailSubtree_OnRefs(t *testing.T) {
	b := New(t, Leyline)
	b.Ref("log", "a.go/fn_0", "", "", Detail{Subtree: "logcall"})
	b.Ref("log", "b.go/fn_0", "", "", Detail{Subtree: "logcall"})
	b.Ref("log", "c.go/fn_0", "", "")
	_, f := b.Build()

	var shared int
	require.NoError(t, f.DB().QueryRow(
		`SELECT COUNT(*) FROM node_refs WHERE node_hash = (
		    SELECT node_hash FROM node_refs WHERE node_id LIKE 'a.go/%')`).Scan(&shared))
	assert.Equal(t, 2, shared, "two occurrences of one deduped subtree")
}

// TestASTNode_TokenLandsInNodeContent covers Detail.Token and Detail.Subtree.
// v_test_nodes' Rust attribute detection reads the token through the
// `_ast.node_hash -> node_content.token` indirection, so a fixture that stored
// the token anywhere else silently makes that detection unreachable.
func TestASTNode_TokenLandsInNodeContent(t *testing.T) {
	b := New(t, Leyline)
	b.ASTNode("f/attr1", "attribute_item", "f.rs", Bytes(60, 68))
	b.ASTNode("f/attr1/id", "identifier", "f.rs", Bytes(62, 66), Detail{Token: "test"})
	b.ASTNode("f/attr2/id", "identifier", "f.rs", Bytes(72, 76),
		Detail{Token: "test", Subtree: "testattr"})
	_, f := b.Build()

	var tok string
	require.NoError(t, f.DB().QueryRow(
		`SELECT c.token FROM _ast a JOIN node_content c ON c.node_hash = a.node_hash
		  WHERE a.node_id = 'f/attr1/id'`).Scan(&tok))
	assert.Equal(t, "test", tok)

	var span Span
	require.NoError(t, f.DB().QueryRow(
		`SELECT start_byte, end_byte FROM _ast WHERE node_id='f/attr1'`).
		Scan(&span.StartByte, &span.EndByte))
	assert.Equal(t, Bytes(60, 68), span)

	var distinct int
	require.NoError(t, f.DB().QueryRow(
		`SELECT COUNT(DISTINCT node_hash) FROM _ast WHERE node_id LIKE '%/id'`).Scan(&distinct))
	assert.Equal(t, 2, distinct, "only the labelled pair would share; these are two labels")
}

// TestASTNode_FieldLandsInNodeChild covers Detail.Field and the merkle child
// list it lands in. The projection resolves a selector's `field:` labels by
// reading `node_child.field` off the parent's list (mache-91d903), so a fixture
// that put the field anywhere else — or listed children in declaration order
// rather than source order — would make a labelled selector match nodes the
// real parse never would.
func TestASTNode_FieldLandsInNodeChild(t *testing.T) {
	b := New(t, Leyline)
	b.ASTNode("m.go/method_declaration", "method_declaration", "m.go", Bytes(0, 40))
	// Declared out of source order on purpose: ordinal follows the start
	// byte, as tree-sitter orders children, not the declaration order.
	b.ASTNode("m.go/method_declaration/field_identifier", "field_identifier", "m.go",
		Bytes(16, 19), Detail{Token: "Put", Field: "name"})
	b.ASTNode("m.go/method_declaration/parameter_list", "parameter_list", "m.go",
		Bytes(5, 15), Detail{Field: "receiver", Subtree: "paren-store"})
	b.ASTNode("m.go/method_declaration/parameter_list/parameter_declaration", "parameter_declaration", "m.go",
		Bytes(6, 14), Detail{Subtree: "store-decl"})
	// `func (Store) Put(Store)`: the receiver and parameter lists are the
	// same subtree, so they share a hash and ley-line lists their children
	// ONCE — the field on the parent's list is all that tells them apart.
	b.ASTNode("m.go/method_declaration/parameter_list_1", "parameter_list", "m.go",
		Bytes(19, 29), Detail{Field: "parameters", Subtree: "paren-store"})
	b.ASTNode("m.go/method_declaration/parameter_list_1/parameter_declaration", "parameter_declaration", "m.go",
		Bytes(20, 28), Detail{Subtree: "store-decl"})
	_, f := b.Build()

	rows, err := f.DB().Query(`
		SELECT c.ordinal, k.kind, COALESCE(c.field, '')
		  FROM node_child c
		  JOIN _ast a ON a.node_hash = c.parent_hash
		  JOIN node_content k ON k.node_hash = c.child_hash
		 WHERE a.node_id = 'm.go/method_declaration'
		 ORDER BY c.ordinal`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var ord int
		var kind, field string
		require.NoError(t, rows.Scan(&ord, &kind, &field))
		got = append(got, fmt.Sprintf("%d %s %s", ord, kind, field))
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{
		"0 parameter_list receiver",
		"1 field_identifier name",
		"2 parameter_list parameters",
	}, got, "children in start-byte order, each under its field")

	var lists int
	require.NoError(t, f.DB().QueryRow(`
		SELECT COUNT(*) FROM node_child WHERE parent_hash =
		    (SELECT node_hash FROM _ast WHERE node_id = 'm.go/method_declaration/parameter_list')`).
		Scan(&lists))
	assert.Equal(t, 1, lists, "one shared subtree, one child list: a single unfielded parameter_declaration")

	// Every _ast node has its nodes row — the projection JOINs the two — and a
	// leaf's token is on nodes.record, as ley-line writes it.
	var record string
	require.NoError(t, f.DB().QueryRow(
		`SELECT record FROM nodes WHERE id = 'm.go/method_declaration/field_identifier'`).Scan(&record))
	assert.Equal(t, "Put", record)
	var astWithoutNode int
	require.NoError(t, f.DB().QueryRow(
		`SELECT COUNT(*) FROM _ast a LEFT JOIN nodes n ON n.id = a.node_id WHERE n.id IS NULL`).
		Scan(&astWithoutNode))
	assert.Zero(t, astWithoutNode)
}

// TestSameChildList pins the twin check emitNodeChildren makes: two ASTNodes
// under one Subtree label must list the same children, because one hash is
// one subtree. Asserted on the comparison directly for the same reason
// nameMatchesID is — the emitter's Fatalf would end the asserting test.
func TestSameChildList(t *testing.T) {
	h1, h2 := subtreeHash("one"), subtreeHash("two")
	assert.True(t, sameChildList(
		[]childRow{{h1, "name"}, {h2, ""}}, []childRow{{h1, "name"}, {h2, ""}}))
	assert.False(t, sameChildList(
		[]childRow{{h1, "name"}, {h2, ""}}, []childRow{{h2, ""}, {h1, "name"}}), "order is content")
	assert.False(t, sameChildList(
		[]childRow{{h1, "name"}}, []childRow{{h1, "type"}}), "the field is content")
	assert.False(t, sameChildList([]childRow{{h1, ""}}, nil), "arity is content")
}

// TestSourceFile_IsPathMode: a path-mode row has no bytes and a real path,
// which the projection reads from disk — the default shape ley-line writes.
func TestSourceFile_IsPathMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	require.NoError(t, os.WriteFile(path, []byte("package a\n"), 0o644))

	b := New(t, Leyline)
	b.ASTNode("a.go/fn_0", "function_declaration", "a.go", Bytes(0, 10))
	b.SourceFile("a.go", "go", path)
	_, f := b.Build()

	var lang, got string
	var content sql.NullString
	require.NoError(t, f.DB().QueryRow(
		`SELECT language, content, path FROM _source WHERE id='a.go'`).Scan(&lang, &content, &got))
	assert.Equal(t, "go", lang)
	assert.False(t, content.Valid, "path mode stores no bytes")
	assert.Equal(t, path, got, "and overrides the synthetic path ASTNode gave the source")
}

// TestImport_OnlyExistsOnLeyline: _imports is producer output, and fatal_call's
// stdlib-vs-local resolution reads it. A Standalone fixture has no such table,
// so the call is dropped rather than fabricating a shape mache never sees.
func TestImport_OnlyExistsOnLeyline(t *testing.T) {
	bl := New(t, Leyline)
	bl.Import("fmt", "fmt", "a.go")
	_, fl := bl.Build()

	var p string
	require.NoError(t, fl.DB().QueryRow(
		`SELECT path FROM _imports WHERE alias='fmt' AND source_id='a.go'`).Scan(&p))
	assert.Equal(t, "fmt", p)

	bs := New(t, Standalone)
	bs.Import("fmt", "fmt", "a.go")
	_, fs := bs.Build()
	var n int
	require.NoError(t, fs.DB().QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE name='_imports'`).Scan(&n))
	assert.Zero(t, n)
}

// TestLSPDef_UnionsIntoTheBindingArm covers the LSP-enrichment table, which
// ensureCanonicalViews unions into v_defs only when `_lsp_defs.def_token`
// exists. It is available on both producers because it is enrichment output,
// not producer output.
func TestLSPDef_UnionsIntoTheBindingArm(t *testing.T) {
	b := New(t, Standalone)
	b.Def("Validate", "auth/functions/Validate", Function)
	b.LSPDef("Validate", "auth/functions/Validate", "file:///auth/auth.go", 10, 5, 10, 13)
	_, f := b.Build()

	var uri string
	require.NoError(t, f.DB().QueryRow(
		`SELECT def_uri FROM _lsp_defs WHERE def_token='Validate'`).Scan(&uri))
	assert.Equal(t, "file:///auth/auth.go", uri)
}

// TestProducer_IsReadableFromTheBuilder so a table-driven test can branch on the
// producer EXPLICITLY rather than by whatever its DDL implied.
func TestProducer_IsReadableFromTheBuilder(t *testing.T) {
	assert.Equal(t, Leyline, New(t, Leyline).Producer())
	assert.Equal(t, Standalone, New(t, Standalone).Producer())
	assert.Equal(t, "leyline", Leyline.String())
	assert.Equal(t, "standalone", Standalone.String())
}

// TestSource_RedeclarationFillsGaps: ASTNode declares its source implicitly, and
// that implicit declaration must never clobber an explicit one — otherwise
// statement order silently decides whether _source.language is set.
func TestSource_RedeclarationFillsGaps(t *testing.T) {
	b := New(t, Leyline)
	b.ASTNode("a.go/fn_0", "function_declaration", "a.go", Bytes(0, 10))
	b.Source("a.go", "go", "package a\n")
	b.ASTNode("a.go/fn_1", "function_declaration", "a.go", Bytes(11, 20))
	_, f := b.Build()

	var lang string
	var content sql.NullString
	require.NoError(t, f.DB().QueryRow(
		`SELECT language, content FROM _source WHERE id='a.go'`).Scan(&lang, &content))
	assert.Equal(t, "go", lang)
	assert.Equal(t, "package a\n", content.String)
}
