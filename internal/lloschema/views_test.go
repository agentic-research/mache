package lloschema

import (
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/fixturedb"
)

// EnsureViews end to end, against a fixture derived from the real pinned
// producer rather than hand-written DDL.
//
// internal/lint's LLO boundary forbids the latter, and the reason applies with
// full force here: EnsureViews emits a structurally different body per column
// combination, so DDL a test happens to type silently decides which arm runs
// (mache-7555da). fixturedb.Leyline derives the shape from ley-line-open's own
// sqlite_master, so the arm under test is the one production takes.
//
// No RegisterViewInstaller here: this test installs the views explicitly, and
// registering a second wrapper identical in shape to internal/smells' is the
// duplication the gate objects to.
func TestEnsureViews_InstallsTheVocabulary(t *testing.T) {
	b := fixturedb.New(t, fixturedb.Leyline)
	b.Def("Alpha", "pkg/a.go/function_declaration_0", fixturedb.Function)
	_, f := b.Build()

	qg := DB(f.DB())
	require.NoError(t, EnsureViews(qg), "a second install on the same connection must be safe")

	// Each view must exist AND carry the stable column names, because the
	// column list is what every consumer writes against.
	for view, want := range map[string][]string{
		"v_ast":   {"node_id", "source_id", "node_kind"},
		"v_nodes": {"node_id", "parent_id", "name", "kind", "source_file"},
		"v_defs":  {"token", "node_id", "fidelity"},
		"v_refs":  {"referrer_node_id", "node_id", "token"},
	} {
		rows, err := qg.QueryRefs("SELECT * FROM " + view + " LIMIT 0")
		require.NoErrorf(t, err, "view %s must exist after EnsureViews", view)
		cols, err := rows.Columns()
		require.NoError(t, err)
		_ = rows.Close()
		for _, c := range want {
			assert.Containsf(t, cols, c, "%s must expose %s — it is part of the vocabulary", view, c)
		}
	}

	// The def seeded above must be visible through the vocabulary, not merely
	// the views being present: an empty view satisfies the column checks.
	rows, err := qg.QueryRefs("SELECT token FROM v_defs WHERE token = 'Alpha'")
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	require.True(t, rows.Next(), "v_defs must surface a seeded definition, not just exist")
}

// ASTSelect is v_ast's body, usable on any connection as a subquery, and reads
// the same rows from projection-v4 and projection-v6 (node ids aside, which
// are paths on one and nids on the other).
func TestASTSelect_SameRowsFromProjectionV4AndV6(t *testing.T) {
	rowsOf := func(p fixturedb.Producer) []string {
		f := paritySpec(t, p)
		body, err := ASTSelect(DB(f.DB()))
		require.NoError(t, err)
		require.NotEmpty(t, body)
		rows, err := f.DB().Query(`SELECT source_id, node_kind, start_byte, end_byte FROM (` + body + `)`)
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()
		var out []string
		for rows.Next() {
			var src, kind string
			var s, e int
			require.NoError(t, rows.Scan(&src, &kind, &s, &e))
			out = append(out, fmt.Sprintf("%s %s [%d,%d)", src, kind, s, e))
		}
		require.NoError(t, rows.Err())
		slices.Sort(out)
		return out
	}
	v4 := rowsOf(fixturedb.Leyline)
	require.NotEmpty(t, v4)
	assert.Equal(t, v4, rowsOf(fixturedb.LeylineV6))

	// A mache projection has no _ast: nothing to select, said as "".
	_, standalone := fixturedb.New(t, fixturedb.Standalone).Construct("a.go/F").Build()
	body, err := ASTSelect(DB(standalone.DB()))
	require.NoError(t, err)
	assert.Empty(t, body)
}
