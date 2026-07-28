package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// buildLeylineGoProjectionFixture seeds a leyline-_ast-SHAPED db for the
// untested_function port (mache-ba24f3). Unlike the mache-schema fixtures
// (construct-dir paths, `pkg/functions/Foo`), leyline emits tree-sitter
// node-kind paths with NO `functions/` segment — the function-vs-method
// distinction lives in node_defs.canonical_kind (LLO v0.7.5+), and a
// ref's enclosing caller lives in node_refs.container_node_id (v0.7.4+),
// NOT in the ref's own node_id (which is the unique call-SITE leaf).
//
// The fixture carries every over-report class the raw port produced
// (757 findings on the mache repo before the fixes, 8 after):
//   - a genuinely uncovered Go function        → the ONE expected finding
//   - a function covered ONLY via a test CALLER (container_node_id join)
//   - a non-Go (Rust) function                 → Go-convention scope
//   - a testdata fixture function              → corpus exclusion
//   - a method (canonical_kind='method')       → kind filter
//   - a non-test caller ref (must NOT count as coverage)
func buildLeylineGoProjectionFixture(t *testing.T) *smellTestGraph {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "leyline_go.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	// Leyline column shape: node_defs carries canonical_kind, node_refs
	// carries container_node_id — the probes in ensureCanonicalViews
	// activate the leyline arms only when these columns exist.
	_, err = db.Exec(`
		CREATE TABLE nodes (
			id TEXT PRIMARY KEY, parent_id TEXT, name TEXT NOT NULL,
			kind INTEGER NOT NULL, size INTEGER DEFAULT 0,
			mtime INTEGER NOT NULL DEFAULT 0, record_id TEXT, record TEXT,
			source_file TEXT
		);
		CREATE TABLE node_defs (
			token TEXT, node_id TEXT, source_id TEXT,
			node_hash BLOB, canonical_kind TEXT,
			PRIMARY KEY (token, node_id)
		) WITHOUT ROWID;
		CREATE TABLE node_refs (
			token TEXT, node_id TEXT, source_id TEXT,
			node_hash BLOB, container_node_id TEXT,
			PRIMARY KEY (token, node_id)
		) WITHOUT ROWID;
	`)
	require.NoError(t, err)

	type def struct{ token, nodeID, sourceFile, kind string }
	defs := []def{
		// Uncovered exported Go function — the one expected finding.
		{"Orphan", "pkg/orphan.go/function_declaration_0", "pkg/orphan.go", "function"},
		// Covered ONLY by a test caller: no TestServed def exists, so
		// coverage must come from the container_node_id join arm.
		{"Served", "pkg/served.go/function_declaration_0", "pkg/served.go", "function"},
		// The test function that calls Served.
		{"TestServedEndToEnd", "pkg/served_test.go/function_declaration_0", "pkg/served_test.go", "function"},
		// The non-test caller that refs Orphan (must NOT grant coverage).
		{"runAll", "pkg/run.go/function_declaration_0", "pkg/run.go", "function"},
		// Rust function: canonical_kind='function' too, but the TestFoo
		// convention is Go-only — the .go scope must exclude it.
		{"RustHelper", "src/lib.rs/function_item_0", "src/lib.rs", "function"},
		// testdata corpus: Go by extension, excluded by directory.
		{"FixtureFn", "testdata/sample.go/function_declaration_0", "testdata/sample.go", "function"},
		{"NestedFixtureFn", "internal/x/testdata/deep.go/function_declaration_0", "internal/x/testdata/deep.go", "function"},
		// Method: uppercase, uncovered, but canonical_kind='method'.
		{"Handle", "pkg/recv.go/method_declaration_0", "pkg/recv.go", "method"},
	}
	for _, d := range defs {
		_, err = db.Exec("INSERT INTO node_defs (token, node_id, source_id, canonical_kind) VALUES (?, ?, ?, ?)",
			d.token, d.nodeID, d.sourceFile, d.kind)
		require.NoError(t, err)
		_, err = db.Exec("INSERT INTO nodes (id, parent_id, name, kind, mtime, record, source_file) VALUES (?, '', ?, 1, 0, '', ?)",
			d.nodeID, d.token, d.sourceFile)
		require.NoError(t, err)
	}

	type ref struct{ token, nodeID, container string }
	refs := []ref{
		// Served is called from inside TestServedEndToEnd: the ref's own
		// node_id is the call-site LEAF (never a def node_id, and never
		// matching the tree-sitter '%/Test%/source' path shapes) — the
		// caller identity is ONLY recoverable via container_node_id.
		{"Served", "pkg/served_test.go/function_declaration_0/block/statement_list/expression_statement/call_expression", "pkg/served_test.go/function_declaration_0"},
		// Orphan is called — but from a NON-test function. Must not count.
		{"Orphan", "pkg/run.go/function_declaration_0/block/statement_list/expression_statement/call_expression", "pkg/run.go/function_declaration_0"},
	}
	for _, r := range refs {
		_, err = db.Exec("INSERT INTO node_refs (token, node_id, source_id, container_node_id) VALUES (?, ?, '', ?)",
			r.token, r.nodeID, r.container)
		require.NoError(t, err)
	}

	return &smellTestGraph{MemoryStore: graph.NewMemoryStore(), db: db}
}

// TestFindSmells_UntestedFunctionLeylineProjection is the leyline parity
// test for mache-ba24f3: before the port the rule returned 0 on every
// leyline projection (the 'functions/' path filter never matched and the
// test-caller CTE's path shapes never matched), and after the naive
// canonical_kind fix it over-reported (every Rust/Python/testdata
// function flagged for lacking a Go-convention Test<name>). Exactly one
// finding — the genuinely uncovered exported Go function — must survive.
func TestFindSmells_UntestedFunctionLeylineProjection(t *testing.T) {
	tg := buildLeylineGoProjectionFixture(t)

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{
		"rule": "untested_function",
	}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))

	require.Equal(t, 1, resp.Total,
		"only Orphan: Served is covered via its test CALLER (container_node_id join), "+
			"RustHelper is out of Go scope, testdata fns are corpus, Handle is a method, "+
			"and runAll's non-test call to Orphan must not count as coverage")
	assert.Equal(t, "pkg/orphan.go/function_declaration_0", resp.Findings[0].NodeID)
	assert.Equal(t, "pkg/orphan.go", resp.Findings[0].SourceID)
}
