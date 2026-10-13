package lloschema

import (
	"database/sql"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/fixturedb"
)

// The canonical views exist so a consumer can read projection-v4 and
// projection-v6 databases with the same SQL. Until fixturedb had a v6
// producer, the v6 arms of those views had nothing real to run against. This
// builds ONE spec as both shapes and requires the vocabulary to give the same
// answers from each.
//
// v6 node ids are integers, so answers are compared as paths: a v4 node_id IS
// its path, and a v6 nid is mapped back through LLO's own v_node_path, which
// is fine in a test and never on a hot path (mache-90cb9f).

func paritySpec(t *testing.T, p fixturedb.Producer) *fixturedb.FixtureDB {
	b := fixturedb.New(t, p)
	b.Source("main.go", "go", "")
	b.Source("pkg/a.go", "go", "")
	b.ASTNode("main.go/function_declaration", "function_declaration", "main.go", fixturedb.Bytes(42, 82))
	b.ASTNode("pkg/a.go/function_declaration_0", "function_declaration", "pkg/a.go", fixturedb.Bytes(40, 110))
	b.ASTNode("pkg/a.go/function_declaration_1", "function_declaration", "pkg/a.go", fixturedb.Bytes(112, 142))
	b.ASTNode("pkg/a.go/function_declaration_0/identifier", "identifier", "pkg/a.go",
		fixturedb.Bytes(45, 50), fixturedb.Detail{Token: "Greet"})
	b.Def("main", "main.go/function_declaration", fixturedb.Function)
	b.Def("Greet", "pkg/a.go/function_declaration_0", fixturedb.Function)
	b.Def("helper", "pkg/a.go/function_declaration_1", fixturedb.Function)
	b.Ref("Greet", "main.go/function_declaration", "", "pkg")
	b.Ref("helper", "pkg/a.go/function_declaration_0", "", "")
	_, f := b.Build()
	require.NoError(t, EnsureViews(DB(f.DB())))
	return f
}

// pathResolver turns a vocabulary node_id into a path: itself on v4, and
// through v_node_path on v6.
func pathResolver(t *testing.T, db *sql.DB, v6 bool) func(any) string {
	return func(id any) string {
		t.Helper()
		if !v6 {
			return fmt.Sprint(id)
		}
		var p string
		require.NoError(t, db.QueryRow(`SELECT path FROM v_node_path WHERE nid = ?`, id).Scan(&p))
		return p
	}
}

// answers is what a consumer reads through the vocabulary, as sorted lines.
func answers(t *testing.T, f *fixturedb.FixtureDB, v6 bool) map[string][]string {
	db := f.DB()
	path := pathResolver(t, db, v6)
	// Rows are read in full and closed BEFORE any path is resolved: the
	// fixture pins one connection (the views are per-connection TEMP views),
	// so a lookup while rows are open would wait forever for it.
	collect := func(query string, render func(cols []any) string) []string {
		rows, err := db.Query(query)
		require.NoError(t, err, query)
		cols, err := rows.Columns()
		require.NoError(t, err)
		var all [][]any
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			require.NoError(t, rows.Scan(ptrs...))
			all = append(all, vals)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		out := make([]string, 0, len(all))
		for _, vals := range all {
			out = append(out, render(vals))
		}
		slices.Sort(out)
		return out
	}
	declared := map[string]bool{
		"main.go/function_declaration": true, "pkg/a.go/function_declaration_0": true,
		"pkg/a.go/function_declaration_1": true, "pkg/a.go/function_declaration_0/identifier": true,
	}
	return map[string][]string{
		"defs": collect(`SELECT token, node_id FROM v_defs`, func(c []any) string {
			return fmt.Sprintf("%s @ %s", c[0], path(c[1]))
		}),
		// The referrer, not the site: a site the spec left unnamed is
		// ".../call_expression_0" in a v4 fixture and the leyline-faithful
		// ".../call_expression" in v6, by design.
		"refs": collect(`SELECT token, referrer_node_id FROM v_refs`, func(c []any) string {
			return fmt.Sprintf("%s from %s", c[0], path(c[1]))
		}),
		"nodes": filter(collect(`SELECT node_id, name, kind FROM v_nodes`, func(c []any) string {
			return fmt.Sprintf("%s name=%v kind=%v", path(c[0]), str(c[1]), c[2])
		}), declared),
		"ast": collect(`SELECT node_id, node_kind FROM v_ast`, func(c []any) string {
			return fmt.Sprintf("%s is %v", path(c[0]), str(c[1]))
		}),
	}
}

func str(v any) any {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return v
}

// filter keeps the lines about declared constructs: a v6 database also holds
// the directories, files and intermediate nodes a v4 fixture never writes.
func filter(lines []string, declared map[string]bool) []string {
	var out []string
	for _, l := range lines {
		for p := range declared {
			if len(l) > len(p) && l[:len(p)] == p && l[len(p)] == ' ' {
				out = append(out, l)
			}
		}
	}
	slices.Sort(out)
	return out
}

func TestVocabulary_SameAnswersFromProjectionV4AndV6(t *testing.T) {
	v4 := answers(t, paritySpec(t, fixturedb.Leyline), false)
	v6 := answers(t, paritySpec(t, fixturedb.LeylineV6), true)

	for _, view := range []string{"defs", "refs", "nodes", "ast"} {
		require.NotEmpty(t, v4[view], "%s: the v4 fixture must surface rows, or parity is vacuous", view)
		assert.Equal(t, v4[view], v6[view], "%s must read the same from projection-v4 and projection-v6", view)
	}
}
