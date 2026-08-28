package leylinegraph

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/agentic-research/mache/internal/sqlintro"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// storedParentIDColumn is what nodes.parent_id was before ley-line-open's
// projection-v4 made it derived, and what it becomes again when
// ley-line-open-17c271 replaces it with a stored integer FK. mache must handle
// both, so the tests below build both.
//
// This is the ONLY DDL text this file authors. Everything else about the table
// — every other column, its order, its types — comes from the fixture's actual
// `nodes` DDL, which internal/fixturedb derives from the pinned producer.
const storedParentIDColumn = "parent_id TEXT"

// buildLeylineFixture returns the path to a ley-line-shaped fixture carrying two
// function constructs where ProcessOrder calls HandleRequest — enough for
// materializeCallers and materializeCallees to have something to project.
//
// Since the v0.19.0 re-pin this fixture's parent_id is GENERATED, because that
// is what the pinned producer writes.
//
// node_defs / node_refs come from fixturedb rather than hand-written DDL: their
// column set decides which arm of ensureCanonicalViews runs, and node_id means
// different things on the two producers, so a fixture that invents a shape is a
// hidden test parameter (internal/lint's LLO boundary rule, mache-7555da).
func buildLeylineFixture(t *testing.T) string {
	t.Helper()
	path, _ := fixturedb.New(t, fixturedb.Leyline).
		Construct("functions", fixturedb.Where{Directory: true}).
		Construct("functions/HandleRequest", fixturedb.Where{Parent: "functions", Directory: true}).
		Construct("functions/ProcessOrder", fixturedb.Where{Parent: "functions", Directory: true}).
		Def("HandleRequest", "functions/HandleRequest", fixturedb.Function).
		Def("ProcessOrder", "functions/ProcessOrder", fixturedb.Function).
		Ref("HandleRequest", "functions/ProcessOrder", "functions/ProcessOrder/call_0", "").
		Build()
	return path
}

// migrateNodesToStoredParent rewrites the fixture's `nodes` table so parent_id
// is an ordinary stored column, giving the pre-v4 shape to compare against.
//
// The direction is deliberately this way round since the v0.19.0 re-pin: the
// fixture's own DDL is now the DERIVED one, so the stored arm is the synthetic
// one. The new DDL is DERIVED from the table already present — read out of
// sqlite_master and rewritten at one column — so it follows the pinned producer
// instead of pinning a stale copy. The stored column is populated FROM the
// derived values, which is what makes the parity assertion meaningful: both
// arms carry the parent the producer would have computed.
//
// A missing substitution target fails loudly rather than quietly leaving the
// table generated, which would make the parity assertion vacuous by comparing
// two identical shapes.
func migrateNodesToStoredParent(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	var create string
	require.NoError(t, db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='nodes'`).Scan(&create))

	// Located by span rather than by pattern: mache treats regex as a smell
	// (there is an import ratchet), and the two anchors here are exact literals
	// the producer emits.
	const genPrefix, genSuffix = "parent_id TEXT GENERATED ALWAYS AS (", ") VIRTUAL"
	i := strings.Index(create, genPrefix)
	require.GreaterOrEqual(t, i, 0,
		"the pinned nodes DDL no longer declares a GENERATED parent_id, so this migration "+
			"cannot find what to replace — re-derive it rather than skipping:\n%s", create)
	j := strings.Index(create[i:], genSuffix)
	require.GreaterOrEqual(t, j, 0, "unterminated GENERATED column in:\n%s", create)
	stored := create[:i] + storedParentIDColumn + create[i+j+len(genSuffix):]

	var indexes []string
	rows, err := db.Query(
		`SELECT sql FROM sqlite_master WHERE type='index' AND tbl_name='nodes' AND sql IS NOT NULL`)
	require.NoError(t, err)
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		indexes = append(indexes, s)
	}
	require.NoError(t, rows.Err())
	_ = rows.Close()

	// Carry every column over, parent_id INCLUDED — reading the derived value
	// out and storing it is precisely the equivalence under test.
	cols, err := db.Query(`SELECT name FROM pragma_table_xinfo('nodes') WHERE hidden IN (0, 2, 3)`)
	require.NoError(t, err)
	var names []string
	for cols.Next() {
		var n string
		require.NoError(t, cols.Scan(&n))
		names = append(names, n)
	}
	require.NoError(t, cols.Err())
	_ = cols.Close()
	list := strings.Join(names, ", ")

	stmts := append([]string{
		`ALTER TABLE nodes RENAME TO nodes_v4`,
		stored,
		fmt.Sprintf(`INSERT INTO nodes (%s) SELECT %s FROM nodes_v4`, list, list),
		`DROP TABLE nodes_v4`,
	}, indexes...)
	for _, s := range stmts {
		_, err := db.Exec(s)
		require.NoError(t, err, "migration step failed:\n%s", s)
	}

	var hidden int
	require.NoError(t, db.QueryRow(
		`SELECT hidden FROM pragma_table_xinfo('nodes') WHERE name='parent_id'`).Scan(&hidden))
	require.Equal(t, 0, hidden, "parent_id must be an ordinary stored column after migration")
}

// dumpTree reads every node as (id, parent_id, name, kind), the tuple the NFS
// and MCP layers navigate by.
func dumpTree(t *testing.T, path string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	rows, err := db.Query(
		`SELECT id, COALESCE(parent_id, ''), name, kind FROM nodes ORDER BY id`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var id, parent, name string
		var kind int
		require.NoError(t, rows.Scan(&id, &parent, &name, &kind))
		out = append(out, fmt.Sprintf("%s|%s|%s|%d", id, parent, name, kind))
	}
	require.NoError(t, rows.Err())
	return out
}

// TestMaterializeVirtuals_ParityAcrossParentIDShapes is the load-bearing
// assertion for ley-line-open's projection-v4 (mache-bc6ca3).
//
// MaterializeVirtuals is handed whatever .db the caller points at — openDBGraph
// runs it on any source `mache serve` is given, which includes an arena built
// by `mache build --backend leyline`. When parent_id became a GENERATED column
// there, every INSERT naming it started failing at PREPARE time.
//
// Parity, not merely "does not error": the two shapes must produce the SAME
// tree. A test that only asserted success would pass against an implementation
// that wrote the virtual nodes with wrong or empty parents, which is precisely
// the silent failure a derived parent_id introduces — a wrong parent surfaces
// as an empty directory, never as an error.
func TestMaterializeVirtuals_ParityAcrossParentIDShapes(t *testing.T) {
	schema := &api.Topology{
		Version: "v1",
		Nodes:   []api.Node{{Name: "functions", Selector: "(function_declaration)"}},
	}

	derived := buildLeylineFixture(t) // the pinned producer's own shape
	stored := buildLeylineFixture(t)
	migrateNodesToStoredParent(t, stored)

	require.NoError(t, MaterializeVirtuals(stored, schema, true),
		"stored parent_id: the shape that already worked")
	require.NoError(t, MaterializeVirtuals(derived, schema, true),
		"derived parent_id: an INSERT naming a generated column fails at prepare time")

	storedTree, derivedTree := dumpTree(t, stored), dumpTree(t, derived)

	assert.Equal(t, storedTree, derivedTree,
		"projection-v4 must project the identical tree; a difference here is a node "+
			"reachable under one shape and orphaned under the other")

	// Guard against both sides being trivially equal: the virtual nodes the
	// function exists to create must actually be present, correctly parented.
	assert.Contains(t, derivedTree, "_schema.json||_schema.json|0",
		"root virtual node must derive the empty parent")
	assert.Contains(t, derivedTree, "functions/HandleRequest/callers|functions/HandleRequest|callers|1",
		"callers/ dir must derive its parent from its own id")
	assert.Contains(t, derivedTree, "functions/HandleRequest/callers/ProcessOrder|functions/HandleRequest/callers|ProcessOrder|0",
		"a nested caller entry must derive the callers/ dir as its parent")
}

// TestNodesParentIsGenerated_DistinguishesTheTwoShapes pins the probe that
// selects between them. pragma_table_info is the wrong instrument and cannot
// be substituted: it omits generated columns entirely, so it reports parent_id
// ABSENT for the derived shape rather than derived.
func TestNodesParentIsGenerated_DistinguishesTheTwoShapes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		migrate bool
		want    bool
	}{
		{"generated column (the pinned shape)", false, true},
		{"stored column", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := buildLeylineFixture(t)
			if tc.migrate {
				migrateNodesToStoredParent(t, path)
			}
			db, err := sql.Open("sqlite", path)
			require.NoError(t, err)
			defer func() { _ = db.Close() }()

			tx, err := db.Begin()
			require.NoError(t, err)
			defer func() { _ = tx.Rollback() }()

			assert.Equal(t, tc.want, sqlintro.ColumnIsGenerated(tx, "nodes", "parent_id"))

			// The instrument this replaced: table_info sees the stored column
			// and misses the generated one, so it cannot tell them apart.
			var infoSees int
			require.NoError(t, tx.QueryRow(
				`SELECT COUNT(*) FROM pragma_table_info('nodes') WHERE name='parent_id'`).Scan(&infoSees))
			assert.Equal(t, tc.want, infoSees == 0,
				"table_info reports a generated column as absent — the reason this probe uses xinfo")
		})
	}
}

// TestCheckParentDerivable_MatchesTheSQLDerivation covers the silent-corruption
// half of projection-v4, and pins the Go check against the SQL it stands in for
// rather than against my reading of it.
//
// The generated expression strips the trailing "/"+name from the id, so a row
// whose stored parent disagrees with that takes a parent nobody wrote — usually
// ” — and its siblings quietly vanish from the listing. ley-line-open found one
// such row in their own fixtures (id='root/tricky', name='line1\nline2') and its
// directory went empty.
//
// Each case is decided TWICE: once by checkParentDerivable, and once by running
// ley-line-open's actual generated-column expression in SQLite and comparing the
// result to the stored parent. Writing this caught my own wrong expectation
// twice — see the two annotated cases below.
func TestCheckParentDerivable_MatchesTheSQLDerivation(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// The projection-v4 expression, verbatim, as a standalone query.
	const derive = `SELECT CASE WHEN length(?1) > length(?2)
	                            THEN substr(?1, 1, length(?1) - length(?2) - 1)
	                            ELSE '' END`

	for _, tc := range []struct {
		name             string
		id, parent, node string
		wantErr          bool
	}{
		{"root: id equals name", "functions", "", "functions", false},
		{"child: id is parent + / + name", "a/b", "a", "b", false},
		{"deep child", "a/b/c", "a/b", "c", false},
		// Accepted: a separator inside the name is fine when the derivation
		// still reproduces "a". The invariant is agreement between stored and
		// derived, not separator-free names.
		{"name contains a separator but still derives", "a/b/c", "a", "b/c", false},
		{"name is not the last segment", "root/tricky", "root", "line1\nline2", true},
		{"root whose name differs from its id", "root/tricky", "", "tricky", true},
		{"parent is not a prefix of id", "a/b", "z", "b", true},
		// Rejected here but ACCIDENTALLY tolerated by the SQL: the CASE falls to
		// its ELSE arm whenever name is longer than id, yielding '' — which
		// happens to equal the stored root parent. The row is still incoherent
		// (addressed "a", reports its name as "aaa"), and the bead's acceptance
		// criterion is the structural one, so the stricter check stands. This
		// asymmetry is why the assertion below is one-directional.
		{"name longer than id: SQL tolerates, we do not", "a", "", "aaa", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkParentDerivable(tc.id, tc.parent, tc.node)

			var derived string
			require.NoError(t, db.QueryRow(derive, tc.id, tc.node).Scan(&derived))

			// SOUNDNESS, one direction only: everything the Go check accepts,
			// the SQL derivation must reproduce.
			if err == nil {
				assert.Equal(t, tc.parent, derived,
					"checkParentDerivable accepted a row the derivation disagrees with")
			}

			if tc.wantErr {
				require.Error(t, err, "a row whose parent cannot be derived must be refused, not written")
				assert.Contains(t, err.Error(), tc.id, "the error must name the offending row")
				return
			}
			assert.NoError(t, err)
		})
	}
}
