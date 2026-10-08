package lloschema

import (
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

	qg := sqlQuerier{db: f.DB()}
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
