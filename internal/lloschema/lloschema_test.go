package lloschema

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"
)

// sqlQuerier adapts a *sql.DB to Querier. Deliberately not importing a graph
// backend: this package must be testable without one, which is the same
// property that lets `graph` depend on it.
type sqlQuerier struct{ db *sql.DB }

func (q sqlQuerier) QueryRefs(query string, args ...any) (*sql.Rows, error) {
	return q.db.Query(query, args...)
}

// Moved here with its function (mache-be17ce). It predates this package and
// already documented the table_xinfo rationale below — which is worth saying
// plainly: that trap was known and guarded, and this test is what would have
// caught the "cleaner" parameterised rewrite I nearly shipped during the move.
func TestTableHasColumn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "probe.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// `derived` is a GENERATED column: readable, indexable, returned by
	// SELECT *, and INVISIBLE to PRAGMA table_info. ley-line-open ships them
	// (source_blobs.byte_len STORED, and nodes.parent_id VIRTUAL from
	// projection-v4, mache-bc6ca3), and TableHasColumn decides whether the
	// binding-fidelity clause is added to a view body — so probing with
	// table_info would drop a usable column's rows on the floor.
	_, err = db.Exec(`CREATE TABLE present (
		a TEXT,
		b INTEGER,
		derived INTEGER GENERATED ALWAYS AS (length(a)) VIRTUAL
	)`)
	require.NoError(t, err)

	qg := sqlQuerier{db: db}

	// Existing table, existing column.
	got, err := TableHasColumn(qg, "present", "a")
	require.NoError(t, err)
	assert.True(t, got)

	// Existing table, missing column — collapses with "table missing"
	// into false; consumer doesn't need to distinguish.
	got, err = TableHasColumn(qg, "present", "missing")
	require.NoError(t, err)
	assert.False(t, got)

	// A GENERATED column is present and usable in a view body, so the probe
	// must report it. PRAGMA table_info omits generated columns entirely and
	// would answer false here; PRAGMA table_xinfo is why this passes.
	got, err = TableHasColumn(qg, "present", "derived")
	require.NoError(t, err)
	assert.True(t, got, "a generated column is readable, so the probe must see it")

	// Missing table — also false (PRAGMA table_xinfo returns 0 rows).
	got, err = TableHasColumn(qg, "absent", "a")
	require.NoError(t, err)
	assert.False(t, got)

	// Injection-defense: invalid identifier rejected.
	_, err = TableHasColumn(qg, "no spaces; DROP TABLE present", "a")
	require.Error(t, err)
}

func TestIsSimpleIdent(t *testing.T) {
	for _, ok := range []string{"nodes", "_ast", "node_refs", "v_ast", "A1_b"} {
		assert.True(t, IsSimpleIdent(ok), "%q is the shape of a real table name", ok)
	}
	for _, bad := range []string{
		"", "nodes; DROP TABLE x", "no des", "node-refs", "nodes'", `"nodes"`, "näme",
	} {
		assert.False(t, IsSimpleIdent(bad), "%q must not reach a PRAGMA unquoted", bad)
	}
}

// identityFor is where every projection-version difference lives, so it is
// where the per-version assertion belongs.
//
// It is a PURE mapping precisely so this test needs no database: internal/lint
// forbids a test hand-writing CREATE TABLE for an LLO-owned table, because the
// DDL a fixture types decides which arm EnsureViews emits — a hidden test
// parameter (mache-7555da). fixturedb derives fixtures from the real pinned
// producer, and has no projection-v6 producer yet, so an end-to-end v6 test
// cannot be written without doing exactly what that gate prohibits.
func TestIdentityFor(t *testing.T) {
	v4 := identityFor(false)
	assert.Equal(t, "node_id", v4.defsNode)
	assert.Equal(t, "node_id", v4.refsNode)
	assert.Equal(t, "container_node_id", v4.refsContainer)

	v6 := identityFor(true)
	assert.Equal(t, "nid", v6.defsNode, "projection-v6 renamed node_defs.node_id to nid")
	assert.Equal(t, "nid", v6.refsNode)
	assert.Equal(t, "container_nid", v6.refsContainer,
		"and container_node_id to container_nid — the only other identity column it moved")

	// The claim worth pinning: identity is the ONLY thing that differs. If a
	// future version moves an additive column too, this stops being true and
	// EnsureViews needs more than one resolved name — which is a design
	// change, not a table edit.
	assert.NotEqual(t, v4, v6, "the two schemas must not resolve identically")
}

// A column name reaching the view body must be a bare identifier: it is
// interpolated into SQL, not parameterised, exactly like the table name in
// TableHasColumn.
func TestIdentityFor_NamesAreSafeToInterpolate(t *testing.T) {
	for _, ids := range []identityColumns{identityFor(false), identityFor(true)} {
		for _, name := range []string{ids.defsNode, ids.refsNode, ids.refsContainer} {
			assert.True(t, IsSimpleIdent(name),
				"%q is interpolated into a view body and must be a bare identifier", name)
		}
	}
}
