package fixturedb

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// v6Spec is the corpus the layout was read from (`leyline parse` v0.20.3 on
// main.go + pkg/a.go), declared with faithful names: one main function in
// main.go, three functions in pkg/a.go so their kind repeats and takes "_k".
func v6Spec(t *testing.T, p Producer) *Builder {
	b := New(t, p)
	b.Source("main.go", "go", "")
	b.Source("pkg/a.go", "go", "")
	b.ASTNode("main.go/function_declaration", "function_declaration", "main.go", Bytes(42, 82))
	b.ASTNode("pkg/a.go/function_declaration_0", "function_declaration", "pkg/a.go", Bytes(40, 110))
	b.ASTNode("pkg/a.go/function_declaration_1", "function_declaration", "pkg/a.go", Bytes(112, 142))
	b.ASTNode("pkg/a.go/function_declaration_2", "function_declaration", "pkg/a.go", Bytes(144, 186))
	b.ASTNode("pkg/a.go/function_declaration_0/identifier", "identifier", "pkg/a.go",
		Bytes(45, 50), Detail{Token: "Greet"})
	b.Def("main", "main.go/function_declaration", Function)
	b.Def("Greet", "pkg/a.go/function_declaration_0", Function)
	b.Def("helper", "pkg/a.go/function_declaration_1", Function)
	b.Def("helper2", "pkg/a.go/function_declaration_2", Function)
	b.Ref("Greet", "main.go/function_declaration", "", "pkg")
	b.Ref("helper", "pkg/a.go/function_declaration_2", "", "")
	return b
}

func pathOf(t *testing.T, db *sql.DB, nid int64) string {
	t.Helper()
	var p string
	require.NoError(t, db.QueryRow(`SELECT path FROM v_node_path WHERE nid = ?`, nid).Scan(&p))
	return p
}

func nidOf(t *testing.T, db *sql.DB, path string) int64 {
	t.Helper()
	var nid int64
	require.NoError(t, db.QueryRow(`SELECT nid FROM v_node_path WHERE path = ?`, path).Scan(&nid),
		"no node renders as %q", path)
	return nid
}

// The invariant everything else rests on: every construct the spec declares
// renders, through LLO's own v_node_path, as exactly its id.
func TestLeylineV6_EveryConstructRendersAsItsID(t *testing.T) {
	b := v6Spec(t, LeylineV6)
	_, f := b.Build()
	for _, id := range b.order {
		nid := nidOf(t, f.DB(), string(id))
		assert.Equal(t, string(id), pathOf(t, f.DB(), nid))
	}
}

// The ids follow the layout a real v0.20.3 parse writes.
func TestLeylineV6_IDsFollowLeyline(t *testing.T) {
	_, f := v6Spec(t, LeylineV6).Build()
	db := f.DB()

	// Directories are -dir_id; the root is -1 with an empty name.
	assert.Equal(t, int64(-1), nidOf(t, db, ""), "the root renders as the empty path")
	assert.Equal(t, int64(-2), nidOf(t, db, "pkg"))

	// Files are file_id<<24, numbered in sorted path order: main.go before pkg/a.go.
	assert.Equal(t, int64(1)<<24, nidOf(t, db, "main.go"))
	assert.Equal(t, int64(2)<<24, nidOf(t, db, "pkg/a.go"))
	var fileID int
	require.NoError(t, db.QueryRow(`SELECT file_id FROM _source WHERE id = 'pkg/a.go'`).Scan(&fileID))
	assert.Equal(t, 2, fileID)

	// Syntax nodes are file_id<<24 | preorder ordinal; the first function
	// comes before its own identifier, which comes before the next function.
	f0 := nidOf(t, db, "pkg/a.go/function_declaration_0")
	id0 := nidOf(t, db, "pkg/a.go/function_declaration_0/identifier")
	f1 := nidOf(t, db, "pkg/a.go/function_declaration_1")
	assert.Equal(t, int64(2), f0>>24)
	assert.Less(t, f0, id0)
	assert.Less(t, id0, f1)

	// Only directories and files store a name; syntax nodes derive theirs.
	var stored sql.NullInt64
	require.NoError(t, db.QueryRow(`SELECT name_id FROM nodes WHERE nid = ?`, f0).Scan(&stored))
	assert.False(t, stored.Valid, "a syntax node's name is derived, not stored")
}

// A definition points at its declaration, a reference at its call site with
// the enclosing definition as container: the meanings a real parse writes.
func TestLeylineV6_DefsAndRefsPointAtNids(t *testing.T) {
	_, f := v6Spec(t, LeylineV6).Build()
	db := f.DB()

	var defNid int64
	require.NoError(t, db.QueryRow(`SELECT nid FROM node_defs WHERE token = 'Greet'`).Scan(&defNid))
	assert.Equal(t, "pkg/a.go/function_declaration_0", pathOf(t, db, defNid))

	var site, container int64
	var qualifier sql.NullString
	require.NoError(t, db.QueryRow(
		`SELECT nid, container_nid, qualifier FROM node_refs WHERE token = 'Greet'`).Scan(&site, &container, &qualifier))
	assert.Equal(t, "main.go/function_declaration", pathOf(t, db, container))
	assert.Equal(t, "pkg", qualifier.String)
	// One unnamed call in the function: leyline names it without a suffix.
	assert.Equal(t, "main.go/function_declaration/block/statement_list/expression_statement/call_expression",
		pathOf(t, db, site))
}

// Two unnamed calls in one function become two statements, suffixed because
// the kind now repeats, rather than two rows on one node.
func TestLeylineV6_UnnamedCallsGetTheirOwnNodes(t *testing.T) {
	b := New(t, LeylineV6)
	b.Source("a.go", "go", "")
	b.Ref("x", "a.go/function_declaration", "", "")
	b.Ref("y", "a.go/function_declaration", "", "")
	_, f := b.Build()

	rows, err := f.DB().Query(`SELECT p.path FROM node_refs r JOIN v_node_path p ON p.nid = r.nid ORDER BY r.token`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var p string
		require.NoError(t, rows.Scan(&p))
		got = append(got, p)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{
		"a.go/function_declaration/block/statement_list/expression_statement_0/call_expression",
		"a.go/function_declaration/block/statement_list/expression_statement_1/call_expression",
	}, got)
}

// The guard, asserted on the tree rather than through Build, whose Fatalf
// would end the test making the assertion.
func TestLeylineV6_RefusesNamesLeylineCannotProduce(t *testing.T) {
	tree := func(ids ...string) *v6Tree {
		tr := newV6Tree()
		for _, id := range ids {
			_, err := tr.add(id, "a.go", -1)
			require.NoError(t, err)
		}
		tr.assignIDs()
		return tr
	}

	// A suffix on a kind that does not repeat: leyline writes it bare.
	assert.Equal(t, []string{"a.go/function_declaration_0 renders as a.go/function_declaration"},
		tree("a.go/function_declaration_0").nameProblems())

	// A bare name on a kind that does repeat: leyline suffixes both.
	assert.NotEmpty(t, tree("a.go/function_declaration", "a.go/function_declaration_1").nameProblems())

	// The faithful shapes pass.
	assert.Empty(t, tree("a.go/function_declaration").nameProblems())
	assert.Empty(t, tree("a.go/function_declaration_0", "a.go/function_declaration_1").nameProblems())

	// An id that is not under its source cannot be placed at all.
	_, err := newV6Tree().add("b.go/function_declaration", "a.go", -1)
	assert.Error(t, err)
}

func TestSplitKind(t *testing.T) {
	for _, tc := range []struct {
		seg, kind string
		k         int
	}{
		{"function_declaration_2", "function_declaration", 2},
		{"block", "block", -1},
		{"expression_statement_0", "expression_statement", 0},
		{"call_", "call_", -1},
		{"_7", "_7", -1},
	} {
		kind, k := splitKind(tc.seg)
		assert.Equal(t, tc.kind, kind, tc.seg)
		assert.Equal(t, tc.k, k, tc.seg)
	}
}
