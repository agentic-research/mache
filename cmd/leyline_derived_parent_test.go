package cmd

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// generatedParentIDColumn is ley-line-open's projection-v4 definition of
// nodes.parent_id (mache-bc6ca3): the row's own id with the trailing "/"+name
// removed, computed rather than stored.
//
// This is the ONLY DDL text this test authors. Everything else about the table
// — every other column, its order, its types — is taken from the fixture's
// actual `nodes` DDL, which internal/fixturedb derives from the pinned
// producer. Retyping the whole CREATE TABLE would reintroduce the drift that
// package exists to remove; substituting one column keeps the rest pinned.
const generatedParentIDColumn = `parent_id TEXT GENERATED ALWAYS AS (
	CASE WHEN length(id) > length(name)
	     THEN substr(id, 1, length(id) - length(name) - 1)
	     ELSE '' END
) VIRTUAL`

// buildLeylineFixture returns the path to a ley-line-shaped fixture carrying
// two function constructs where ProcessOrder calls HandleRequest — enough for
// materializeCallers and materializeCallees to have something to project.
//
// node_defs / node_refs come from fixturedb rather than hand-written DDL: their
// column set decides which arm of ensureCanonicalViews runs, and node_id means
// different things on the two producers, so a fixture that invents a shape is a
// hidden test parameter (see internal/lint's LLO boundary rule, mache-7555da).
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

// migrateNodesToDerivedParent rewrites the fixture's `nodes` table into the
// projection-v4 shape, using the same steps ley-line-open's own migration takes:
// rename aside, create from the new DDL, copy the shared columns, drop the old
// table, rebuild the indexes.
//
// The new DDL is DERIVED from the table already present — read back out of
// sqlite_master and rewritten at one column — so if the pinned producer's
// `nodes` shape ever changes, this follows it instead of pinning a stale copy.
// A missing substitution target fails loudly rather than silently producing a
// table with a stored parent_id, which would make the parity assertion vacuous.
func migrateNodesToDerivedParent(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	var create string
	require.NoError(t, db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='nodes'`).Scan(&create))

	const storedCol = "parent_id TEXT,"
	require.Contains(t, create, storedCol,
		"the pinned nodes DDL no longer declares a plain `parent_id TEXT` column, so this "+
			"migration cannot find what to replace — re-derive it rather than skipping")
	generated := strings.Replace(create, storedCol, generatedParentIDColumn+",", 1)

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

	// The columns to carry over: everything the new table declares except the
	// derived one, which cannot be named in an INSERT.
	cols, err := db.Query(`SELECT name FROM pragma_table_xinfo('nodes') WHERE hidden = 0`)
	require.NoError(t, err)
	var names []string
	for cols.Next() {
		var n string
		require.NoError(t, cols.Scan(&n))
		if n != "parent_id" {
			names = append(names, n)
		}
	}
	require.NoError(t, cols.Err())
	_ = cols.Close()
	list := strings.Join(names, ", ")

	stmts := append([]string{
		`ALTER TABLE nodes RENAME TO nodes_pre_v4`,
		generated,
		fmt.Sprintf(`INSERT INTO nodes (%s) SELECT %s FROM nodes_pre_v4`, list, list),
		`DROP TABLE nodes_pre_v4`,
	}, indexes...)
	for _, s := range stmts {
		_, err := db.Exec(s)
		require.NoError(t, err, "migration step failed:\n%s", s)
	}

	// The migration is only a valid setup for the parity test if it actually
	// produced a generated column; otherwise both arms would test one shape.
	var hidden int
	require.NoError(t, db.QueryRow(
		`SELECT hidden FROM pragma_table_xinfo('nodes') WHERE name='parent_id'`).Scan(&hidden))
	require.Equal(t, 2, hidden, "parent_id must be GENERATED VIRTUAL after migration")
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
// materializeVirtuals is handed whatever .db the caller points at — openDBGraph
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

	stored := buildLeylineFixture(t)
	derived := buildLeylineFixture(t)
	migrateNodesToDerivedParent(t, derived)

	require.NoError(t, materializeVirtuals(stored, schema, true),
		"stored parent_id: the shape that already worked")
	require.NoError(t, materializeVirtuals(derived, schema, true),
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
		{"stored column", false, false},
		{"generated column", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := buildLeylineFixture(t)
			if tc.migrate {
				migrateNodesToDerivedParent(t, path)
			}
			db, err := sql.Open("sqlite", path)
			require.NoError(t, err)
			defer func() { _ = db.Close() }()

			tx, err := db.Begin()
			require.NoError(t, err)
			defer func() { _ = tx.Rollback() }()

			assert.Equal(t, tc.want, nodesParentIsGenerated(tx))

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
