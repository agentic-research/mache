package ingest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/stretchr/testify/require"
)

// TestSQLiteGraph_GetCallees_NodeRefsFallback_RealSchemaBuild is the REAL
// end-to-end regression test for bead mache-6fbaf1: find_callees silently
// returned [] on every default `mache serve` .db path because the scoped
// _ast fix (mache-fd9982, ast_walker_callees_e2e_test.go) needs BOTH
// persisted node Properties (ast_source_id/ast_scope_id) AND a live `_ast`
// table, but the two never coexist on a served .db — a `--schema` build has
// Properties but retains NO `_ast` (228MB+162MB for the whole repo is
// prohibitive), so pickScopedCallExtractor/pickCallExtractor wire up a
// nil/no-op extractor and GetCallees's legacy content path silently
// returns nothing.
//
// Unlike the hand-built _ast fixtures in ast_walker_callees_e2e_test.go
// (which would pass even with this whole gap, since they hand-insert `_ast`
// and hand-set Properties), this test drives the REAL production path:
// leyline parse -> ingest.Engine -> ingest.SQLiteWriter, exactly what
// `mache build --schema` runs, producing a .db with node_refs/node_defs but
// deliberately NO `_ast` table. It then opens that .db as a bare
// graph.SQLiteGraph (no SetScopedCallExtractor, no SetCallExtractor wired)
// — the exact shape `mache serve some.db` exposes — and asserts GetCallees
// resolves the real call through the node_refs fallback.
//
// It also asserts the F3 fix: a grouping/package-level directory (which
// carries no per-construct AST scope) must not return a whole-file call
// union, and its persisted Properties must not carry a whole-file
// ast_scope_id.
func TestSQLiteGraph_GetCallees_NodeRefsFallback_RealSchemaBuild(t *testing.T) {
	schema := loadGoSchema(t)

	srcDir := t.TempDir()
	goFile := filepath.Join(srcDir, "pkg.go")
	src := "package pkg\n\n" +
		"func Helper() int {\n\treturn 1\n}\n\n" +
		"func Caller() int {\n\treturn Helper()\n}\n"
	require.NoError(t, os.WriteFile(goFile, []byte(src), 0o644))

	// Build exactly as `mache build --schema` does: leyline parse -> Engine
	// -> SQLiteWriter. The writer's schema (sqlite_writer.go) has
	// nodes/node_refs/node_defs but NEVER creates an `_ast` table — this is
	// the schema-built-.db shape, not the frozen-auto-leyline-.db shape.
	dbPath := filepath.Join(t.TempDir(), "graph.db")
	writer, err := NewSQLiteWriter(dbPath)
	require.NoError(t, err)

	engine := NewEngine(schema, writer)
	attachLeylineAST(t, engine, srcDir)
	require.NoError(t, engine.Ingest(srcDir))
	require.NoError(t, writer.Close())

	// Confirm the built .db really has no `_ast` table — the whole point of
	// this test is proving the fallback works WITHOUT one.
	rawDB, err := graph.OpenSQLiteGraph(dbPath, schema, nil)
	require.NoError(t, err)
	defer func() { _ = rawDB.Close() }()

	var hasAST int
	require.NoError(t, rawDB.DB().QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='_ast'`,
	).Scan(&hasAST))
	require.Equal(t, 0, hasAST, "schema-built .db must not retain an _ast table (228MB+162MB whole-repo cost)")

	// Deliberately do NOT call SetScopedCallExtractor / SetCallExtractor —
	// this is the bare shape `mache serve graph.db` opens: no pickers can
	// wire a real extractor because there's no `_ast` table to detect.
	sg := rawDB

	t.Run("GetCallees resolves the real call via node_refs, not []", func(t *testing.T) {
		callees, err := sg.GetCallees("pkg/functions/Caller")
		require.NoError(t, err)
		require.NotEmpty(t, callees, "find_callees must not silently return [] on a schema-built .db (bead mache-6fbaf1)")
		var ids []string
		for _, n := range callees {
			ids = append(ids, n.ID)
		}
		require.Contains(t, ids, "pkg/functions/Helper", "Caller's call to Helper() must resolve via node_refs")
	})

	t.Run("F3: grouping dir does not return a whole-file call union", func(t *testing.T) {
		callees, err := sg.GetCallees("pkg/functions")
		require.NoError(t, err)
		require.Empty(t, callees, "a grouping dir (functions/) has no per-construct scope and must not leak the whole file's calls")
	})

	t.Run("F3: package root and grouping dir carry no whole-file ast_scope_id Property", func(t *testing.T) {
		assertNoWholeFileScope := func(t *testing.T, id string) {
			t.Helper()
			var recordJSON string
			err := sg.DB().QueryRow("SELECT record FROM nodes WHERE id = ? AND kind = 1", id).Scan(&recordJSON)
			require.NoError(t, err, "node %s must exist", id)
			n := &graph.Node{Properties: graph.DecodeProps([]byte(recordJSON))}
			srcID := graph.PropString(n, "ast_source_id")
			scopeID := graph.PropString(n, "ast_scope_id")
			if srcID != "" && scopeID != "" {
				require.NotEqual(t, srcID, scopeID,
					"%s must not carry a whole-file ast_scope_id (F3, bead mache-6fbaf1)", id)
			}
		}
		assertNoWholeFileScope(t, "pkg")
		assertNoWholeFileScope(t, "pkg/functions")
	})

	t.Run("regression: real leaf constructs still carry their own AST scope", func(t *testing.T) {
		var recordJSON string
		require.NoError(t, sg.DB().QueryRow(
			"SELECT record FROM nodes WHERE id = ? AND kind = 1", "pkg/functions/Caller",
		).Scan(&recordJSON))
		n := &graph.Node{Properties: graph.DecodeProps([]byte(recordJSON))}
		srcID := graph.PropString(n, "ast_source_id")
		scopeID := graph.PropString(n, "ast_scope_id")
		require.NotEmpty(t, srcID, "a real leaf construct must still get its ast_source_id Property")
		require.NotEmpty(t, scopeID, "a real leaf construct must still get its ast_scope_id Property")
		require.NotEqual(t, srcID, scopeID)
	})
}
