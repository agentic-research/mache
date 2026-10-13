package smells

import (
	"database/sql"
	"path"
	"testing"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/agentic-research/mache/internal/testutil"
	"github.com/stretchr/testify/require"
)

// Tests in this package build their .db fixtures through internal/fixturedb,
// which owns the producer DDL and forbids a test from stating a schema. See
// that package's doc comment for why (mache-7555da).
//
// The canonical views are installed by fixturedb itself, from the installer
// registered here — so no test can build a fixture and forget them, which is the
// failure the "26 tests that build fixtures by hand" note in smell_refs_views.go
// describes.
func init() {
	fixturedb.RegisterViewInstaller(func(qg fixturedb.RefsQuerier) error {
		return EnsureCanonicalViews(qg)
	})
}

// newSmellFixture builds a producer-shaped fixture and wraps it as the graph
// backend the find_smells handlers take.
//
// p is not defaulted and never inferred: every fixture must SAY which producer
// it models, because EnsureCanonicalViews emits a structurally different v_refs
// per producer and the choice decides which SQL the rule under test runs.
func newSmellFixture(t *testing.T, p fixturedb.Producer, seed func(*fixturedb.Builder)) *testutil.SmellTestGraph {
	t.Helper()
	b := fixturedb.New(t, p)
	seed(b)
	path, f := b.Build()
	return &testutil.SmellTestGraph{MemoryStore: graph.NewMemoryStore(), DB: f.DB(), Path: path}
}

// completeMacheDirs adds the directory rows a hand-built mache-schema fixture
// leaves out. mache's writer emits a nodes row for every directory
// (pkg, pkg/types, ...), and the category rules read a definition's ancestry
// through those rows (v_def_scope, mache-2dc105); a fixture that names only
// its leaves describes a projection mache never writes. Ids here are mache
// paths by construction, so a parent is the id up to its last '/'.
func completeMacheDirs(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`SELECT parent_id FROM nodes WHERE parent_id IS NOT NULL AND parent_id != ''`)
	require.NoError(t, err)
	var parents []string
	for rows.Next() {
		var p string
		require.NoError(t, rows.Scan(&p))
		parents = append(parents, p)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	for _, p := range parents {
		for dir := p; dir != "." && dir != ""; dir = path.Dir(dir) {
			parent := path.Dir(dir)
			if parent == "." {
				parent = ""
			}
			_, err := db.Exec(`INSERT OR IGNORE INTO nodes (id, parent_id, name, kind, mtime) VALUES (?, ?, ?, 1, 0)`,
				dir, parent, path.Base(dir))
			require.NoError(t, err)
		}
	}
}
