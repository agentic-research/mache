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

// smellTestGraph is a tiny graph backend that delegates QueryRefs to a
// caller-supplied *sql.DB. It exists only for these tests because we
// don't want to spin up a full SQLiteGraph just to run a SQL pattern.
type smellTestGraph struct {
	*graph.MemoryStore
	db *sql.DB
}

func (s *smellTestGraph) QueryRefs(query string, args ...any) (*sql.Rows, error) {
	return s.db.Query(query, args...)
}

// seedSmellAST creates a minimal _ast database modeling the Go statements
//
//	if x == 42 { ... }       // magic int — should be flagged
//	if y == 0 { ... }        // technically magic too — should be flagged
//	a := someFunc()           // no magic int → should NOT be flagged
//
// We don't represent the full AST — only the binary_expression and
// int_literal rows that the rule needs, plus their parent_id wiring.
func seedSmellAST(t *testing.T) *smellTestGraph {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "smells.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)

	_, err = db.Exec(`
		CREATE TABLE nodes (
			id TEXT PRIMARY KEY, parent_id TEXT, name TEXT NOT NULL,
			kind INTEGER NOT NULL, size INTEGER DEFAULT 0,
			mtime INTEGER NOT NULL, record_id TEXT, record JSON,
			source_file TEXT
		);
		CREATE TABLE _ast (
			node_id TEXT PRIMARY KEY, source_id TEXT NOT NULL,
			node_kind TEXT NOT NULL,
			start_byte INTEGER NOT NULL, end_byte INTEGER NOT NULL,
			start_row INTEGER, start_col INTEGER,
			end_row INTEGER, end_col INTEGER
		);
		CREATE TABLE _source (id TEXT PRIMARY KEY, language TEXT NOT NULL, content BLOB NOT NULL);
		-- node_defs / node_refs are referenced by most rules. Pre-
		-- create empty so tests that don't redefine them don't hit
		-- 'no such table'. Tests that DO seed rows just INSERT;
		-- IF NOT EXISTS protects the test-level CREATE statements
		-- already present in many fixtures.
		CREATE TABLE IF NOT EXISTS node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE IF NOT EXISTS node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
	`)
	require.NoError(t, err)

	const src = "package main\nfunc main() {\n\tx := 1\n\tif x == 42 { return }\n\tif x == 0 { return }\n}\n"
	_, err = db.Exec("INSERT INTO _source VALUES ('main.go', 'go', ?)", []byte(src))
	require.NoError(t, err)

	type row struct {
		id, parentID string
		kind         int
	}
	for _, r := range []row{
		{"src", "", 1},
		{"src/binexpr_42", "src", 1},
		{"src/binexpr_42/lit_42", "src/binexpr_42", 0},
		{"src/binexpr_0", "src", 1},
		{"src/binexpr_0/lit_0", "src/binexpr_0", 0},
		// A bare int literal NOT under a binary_expression — must be skipped.
		{"src/assignment", "src", 1},
		{"src/assignment/lit_1", "src/assignment", 0},
	} {
		_, err = db.Exec("INSERT INTO nodes (id, parent_id, name, kind, mtime, record) VALUES (?, ?, ?, ?, 0, '')",
			r.id, r.parentID, filepath.Base(r.id), r.kind)
		require.NoError(t, err)
	}

	type ast struct {
		id, kind string
		s, e     int
		row, col int
	}
	for _, a := range []ast{
		{"src/binexpr_42", "binary_expression", 38, 45, 3, 4},
		{"src/binexpr_42/lit_42", "int_literal", 43, 45, 3, 9},
		{"src/binexpr_0", "binary_expression", 60, 66, 4, 4},
		{"src/binexpr_0/lit_0", "int_literal", 65, 66, 4, 9},
		{"src/assignment", "short_var_declaration", 20, 26, 2, 1},
		{"src/assignment/lit_1", "int_literal", 25, 26, 2, 6},
	} {
		_, err = db.Exec(
			"INSERT INTO _ast (node_id, source_id, node_kind, start_byte, end_byte, start_row, start_col, end_row, end_col) VALUES (?, 'main.go', ?, ?, ?, ?, ?, 0, 0)",
			a.id, a.kind, a.s, a.e, a.row, a.col,
		)
		require.NoError(t, err)
	}

	return &smellTestGraph{MemoryStore: graph.NewMemoryStore(), db: db}
}

func TestFindSmells_ListsRulesWhenNoRule(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Help  string `json:"help"`
		Rules []struct {
			ID          string   `json:"id"`
			Languages   []string `json:"languages"`
			Description string   `json:"description"`
			Requires    []string `json:"requires"`
		} `json:"rules"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))
	assert.NotEmpty(t, resp.Help)

	byID := make(map[string][]string, len(resp.Rules))
	for _, r := range resp.Rules {
		byID[r.ID] = r.Requires
	}
	require.Contains(t, byID, "magic_int_in_comparison")

	// Every registered rule must declare what tables it reads — the
	// listing is the agent's pre-flight check against the active backend.
	for id, req := range byID {
		assert.NotEmpty(t, req, "rule %q must declare its required tables in `requires`", id)
	}

	// Spot-check a few representatives so the categorization stays honest:
	// _ast rules need leyline parse; node_defs/node_refs rules work on
	// any backend that populates the cross-ref tables. Use require.Contains
	// for the rule-ID lookups so a missing rule fails with a clear message
	// instead of an empty-slice assertion further down.
	for _, id := range []string{"cyclomatic_complexity", "fan_out_skew", "dead_code"} {
		require.Contains(t, byID, id, "rule %q missing from listing", id)
	}
	assert.Contains(t, byID["cyclomatic_complexity"], "_ast",
		"cyclomatic_complexity walks the AST and must require _ast")
	assert.Contains(t, byID["fan_out_skew"], "node_refs",
		"fan_out_skew aggregates over node_refs and must require it")
	assert.Contains(t, byID["dead_code"], "node_refs",
		"dead_code joins defs against refs and must require both tables")
	assert.Contains(t, byID["dead_code"], "node_defs")
}

func TestFindSmells_MagicIntInComparison(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{
		"rule": "magic_int_in_comparison",
	}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Rule     string         `json:"rule"`
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))

	assert.Equal(t, "magic_int_in_comparison", resp.Rule)
	assert.Equal(t, 2, resp.Total, "should flag the two int_literals under binary_expression, not the assignment one")

	// Findings come back sorted by start_byte: lit_42 (byte 43) then lit_0 (byte 65).
	require.Len(t, resp.Findings, 2)
	assert.Equal(t, "main.go", resp.Findings[0].SourceID)
	assert.Equal(t, 4, resp.Findings[0].Line, "tree-sitter row+1 should be 1-based line 4 (3+1)")
	assert.Equal(t, "src/binexpr_42/lit_42", resp.Findings[0].NodeID)
	assert.Contains(t, resp.Findings[0].Snippet, "42")

	assert.Equal(t, "src/binexpr_0/lit_0", resp.Findings[1].NodeID)
	assert.Equal(t, 5, resp.Findings[1].Line)
}

func TestFindSmells_UnknownRuleErrors(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{
		"rule": "no_such_rule",
	}))
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Contains(t, resultText(t, res), "unknown rule")
}

func TestFindSmells_BackendWithoutAstReturnsError(t *testing.T) {
	// MemoryStore implements refsQuerier (its sidecar DB only has
	// node_refs — no _ast). Running the smell rule must surface a
	// clear error rather than crashing or returning empty success.
	store := graph.NewMemoryStore()
	handler := makeFindSmellsHandler(store)
	res, err := handler(context.Background(), makeRequest(map[string]any{
		"rule": "magic_int_in_comparison",
	}))
	require.NoError(t, err)
	require.True(t, res.IsError)
	// We don't pin a specific message — different backends fail at
	// different points (no refs DB, no _ast table, etc.). The
	// invariant is that the user sees an error result.
	assert.NotEmpty(t, resultText(t, res))
}

// TestFindSmells_PreflightFlagsMissingTables ensures that running an
// _ast-required rule against a backend without _ast surfaces a friendly
// error naming the missing table — the agent shouldn't have to parse a
// raw SQL "no such table" string.
func TestFindSmells_PreflightFlagsMissingTables(t *testing.T) {
	// Backend with nodes/node_defs/node_refs but NO _ast table.
	dbPath := filepath.Join(t.TempDir(), "no_ast.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	_, err = db.Exec(`
		CREATE TABLE nodes (id TEXT PRIMARY KEY, parent_id TEXT, name TEXT, kind INTEGER, mtime INTEGER, source_file TEXT, record TEXT);
		CREATE TABLE IF NOT EXISTS node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id));
		CREATE TABLE IF NOT EXISTS node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id));
	`)
	require.NoError(t, err)
	tg := &smellTestGraph{MemoryStore: graph.NewMemoryStore(), db: db}
	defer func() { _ = tg.db.Close() }()

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{
		"rule": "cyclomatic_complexity",
	}))
	require.NoError(t, err)
	require.True(t, res.IsError)

	msg := resultText(t, res)
	assert.Contains(t, msg, "_ast", "error must name the missing table")
	assert.Contains(t, msg, "ley-line-open", "error should point users at LLO docs")

	// Sanity: a rule that only needs node_defs/node_refs/nodes runs
	// fine on the same backend.
	res, err = handler(context.Background(), makeRequest(map[string]any{"rule": "dead_code"}))
	require.NoError(t, err)
	require.False(t, res.IsError, "dead_code only needs node_defs/node_refs/nodes; pre-flight should pass")
}

func TestFindSmells_SourceIDFilterScopes(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	// Add a second source file with one magic int — make sure source_id
	// filter excludes it.
	_, err := tg.db.Exec(`
		INSERT INTO _source VALUES ('other.go', 'go', '');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, record) VALUES
		  ('other_root', '', 'other_root', 1, 0, ''),
		  ('other_root/binexpr', 'other_root', 'binexpr', 1, 0, ''),
		  ('other_root/binexpr/lit', 'other_root/binexpr', 'lit', 0, 0, '');
		INSERT INTO _ast (node_id, source_id, node_kind, start_byte, end_byte, start_row, start_col, end_row, end_col) VALUES
		  ('other_root/binexpr', 'other.go', 'binary_expression', 0, 5, 0, 0, 0, 0),
		  ('other_root/binexpr/lit', 'other.go', 'int_literal', 4, 5, 0, 0, 0, 0);
	`)
	require.NoError(t, err)

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{
		"rule":      "magic_int_in_comparison",
		"source_id": "other.go",
	}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))
	assert.Equal(t, 1, resp.Total)
	assert.Equal(t, "other.go", resp.Findings[0].SourceID)
}

// TestFindSmells_DeadCode seeds node_defs + node_refs and asserts that
// only the unreferenced symbol is flagged. Excludes one symbol on the
// rule's skip list (init) to verify that filter still works.
func TestFindSmells_DeadCode(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		CREATE TABLE IF NOT EXISTS node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE IF NOT EXISTS node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;

		-- Defined + referenced — alive.
		INSERT INTO node_defs VALUES ('LiveFn', 'pkg/funcs/LiveFn');
		INSERT INTO node_refs VALUES ('LiveFn', 'pkg/funcs/Caller/source');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/funcs/LiveFn',         'pkg/funcs',        'LiveFn',  1, 0, 'live.go',   ''),
		  ('pkg/funcs/Caller/source',  'pkg/funcs/Caller', 'source',  0, 0, 'caller.go', '');

		-- Defined, never referenced — DEAD.
		INSERT INTO node_defs VALUES ('Orphan', 'pkg/funcs/Orphan');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/funcs/Orphan', 'pkg/funcs', 'Orphan', 1, 0, 'orphan.go', '');

		-- Defined + on the skip list — must NOT be flagged.
		INSERT INTO node_defs VALUES ('init', 'pkg/funcs/init');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/funcs/init', 'pkg/funcs', 'init', 1, 0, 'startup.go', '');
	`)
	require.NoError(t, err)

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{
		"rule": "dead_code",
	}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))
	require.Equal(t, 1, resp.Total, "only Orphan is unreferenced and not on the skip list")
	assert.Equal(t, "pkg/funcs/Orphan", resp.Findings[0].NodeID)
	assert.Equal(t, "orphan.go", resp.Findings[0].SourceID,
		"source_id comes from the construct dir's source_file column")
}

// TestFindSmells_DeadCodeSkipsImports asserts that imports/ nodes
// don't surface in dead_code, mirroring the same skip in
// duplicate_definitions (#227). Imports are references TO external
// packages; their "tokens" never appear in node_refs because
// node_refs tracks function calls, not import paths.
func TestFindSmells_DeadCodeSkipsImports(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		CREATE TABLE IF NOT EXISTS node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE IF NOT EXISTS node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;

		-- Import token — has no entry in node_refs. The OLD rule would
		-- flag this as dead; the new filter must skip /imports/.
		INSERT INTO node_defs VALUES ('"fmt"', 'pkg/imports/"fmt"');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/imports/"fmt"', 'pkg/imports', '"fmt"', 1, 0, 'main.go', '');

		-- Real dead function — control: must still be flagged.
		INSERT INTO node_defs VALUES ('Orphan', 'pkg/functions/Orphan');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/functions/Orphan', 'pkg/functions', 'Orphan', 1, 0, 'orphan.go', '');
	`)
	require.NoError(t, err)

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{"rule": "dead_code"}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))

	gotIDs := make([]string, len(resp.Findings))
	for i, f := range resp.Findings {
		gotIDs[i] = f.NodeID
	}
	for _, id := range gotIDs {
		assert.NotContains(t, id, "/imports/", "imports/ nodes must not appear in dead_code")
	}
	assert.Equal(t, []string{"pkg/functions/Orphan"}, gotIDs)
}

// TestFindSmells_DeadCodeSkipsNonCallableCategories asserts that
// types/, constants/, and variables/ category nodes don't surface
// in dead_code. node_refs is populated by call-extraction only;
// identifiers used in type / const / variable position never land
// there, so flagging them is pure noise. Surfaced by the explicit
// go-schema path (PR #240's GHA comment showed dozens of FPs like
// 'cmd/types/SmellRule', 'cmd/constants/padding').
func TestFindSmells_DeadCodeSkipsNonCallableCategories(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		CREATE TABLE IF NOT EXISTS node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE IF NOT EXISTS node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;

		-- Non-callable categories — none of these belong in dead_code.
		INSERT INTO node_defs VALUES
		  ('SomeType',  'pkg/types/SomeType'),
		  ('MaxSize',   'pkg/constants/MaxSize'),
		  ('globalCfg', 'pkg/variables/globalCfg');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/types/SomeType',         'pkg/types',     'SomeType',  1, 0, 'a.go', ''),
		  ('pkg/constants/MaxSize',      'pkg/constants', 'MaxSize',   1, 0, 'b.go', ''),
		  ('pkg/variables/globalCfg',    'pkg/variables', 'globalCfg', 1, 0, 'c.go', '');

		-- Real dead function — control: must still be flagged.
		INSERT INTO node_defs VALUES ('Orphan', 'pkg/functions/Orphan');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/functions/Orphan', 'pkg/functions', 'Orphan', 1, 0, 'orphan.go', '');
	`)
	require.NoError(t, err)

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{"rule": "dead_code"}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))

	gotIDs := make([]string, len(resp.Findings))
	for i, f := range resp.Findings {
		gotIDs[i] = f.NodeID
	}
	for _, id := range gotIDs {
		assert.NotContains(t, id, "/types/", "types/ nodes must not appear")
		assert.NotContains(t, id, "/constants/", "constants/ nodes must not appear")
		assert.NotContains(t, id, "/variables/", "variables/ nodes must not appear")
	}
	assert.Equal(t, []string{"pkg/functions/Orphan"}, gotIDs,
		"only the real dead function in functions/ is flagged")
}

// TestFindSmells_DeadCodeSkipsExternalInterfaceMethods asserts that
// methods implementing common external-interface contracts (SQLite
// vtab, billy.Filesystem, net/http handlers) don't surface in
// dead_code. These are dispatched by external runtimes / libraries
// rather than via static call sites in the indexed scope.
func TestFindSmells_DeadCodeSkipsExternalInterfaceMethods(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		CREATE TABLE IF NOT EXISTS node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE IF NOT EXISTS node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;

		-- vtab interface methods (SQLite virtual-table) — must skip.
		INSERT INTO node_defs VALUES
		  ('BestIndex',  'methods/BestIndex'),
		  ('Filter',     'methods/Filter'),
		  ('Eof',        'methods/Eof'),
		  ('Rowid',      'methods/Rowid'),
		  ('Connect',    'methods/Connect');
		-- billy.Filesystem methods — must skip.
		INSERT INTO node_defs VALUES
		  ('Chroot',     'methods/Chroot'),
		  ('Readlink',   'methods/Readlink'),
		  ('TempFile',   'methods/TempFile');
		-- net/http handler — must skip.
		INSERT INTO node_defs VALUES
		  ('ServeHTTP',  'methods/ServeHTTP');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('methods/BestIndex',         'methods', 'BestIndex',  1, 0, '',          ''),
		  ('methods/BestIndex/source',  'methods/BestIndex',  'source', 0, 0, 'a.go', ''),
		  ('methods/Filter',            'methods', 'Filter',     1, 0, '',          ''),
		  ('methods/Filter/source',     'methods/Filter',     'source', 0, 0, 'a.go', ''),
		  ('methods/Eof',               'methods', 'Eof',        1, 0, '',          ''),
		  ('methods/Eof/source',        'methods/Eof',        'source', 0, 0, 'a.go', ''),
		  ('methods/Rowid',             'methods', 'Rowid',      1, 0, '',          ''),
		  ('methods/Rowid/source',      'methods/Rowid',      'source', 0, 0, 'a.go', ''),
		  ('methods/Connect',           'methods', 'Connect',    1, 0, '',          ''),
		  ('methods/Connect/source',    'methods/Connect',    'source', 0, 0, 'a.go', ''),
		  ('methods/Chroot',            'methods', 'Chroot',     1, 0, '',          ''),
		  ('methods/Chroot/source',     'methods/Chroot',     'source', 0, 0, 'b.go', ''),
		  ('methods/Readlink',          'methods', 'Readlink',   1, 0, '',          ''),
		  ('methods/Readlink/source',   'methods/Readlink',   'source', 0, 0, 'b.go', ''),
		  ('methods/TempFile',          'methods', 'TempFile',   1, 0, '',          ''),
		  ('methods/TempFile/source',   'methods/TempFile',   'source', 0, 0, 'b.go', ''),
		  ('methods/ServeHTTP',         'methods', 'ServeHTTP',  1, 0, '',          ''),
		  ('methods/ServeHTTP/source',  'methods/ServeHTTP',  'source', 0, 0, 'h.go', '');

		-- Real dead helper as a control — must still be flagged.
		INSERT INTO node_defs VALUES ('Orphan', 'functions/Orphan');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('functions/Orphan',        'functions',         'Orphan', 1, 0, '',          ''),
		  ('functions/Orphan/source', 'functions/Orphan',  'source', 0, 0, 'orphan.go', '');
	`)
	require.NoError(t, err)

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{"rule": "dead_code"}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))

	gotIDs := make([]string, len(resp.Findings))
	for i, f := range resp.Findings {
		gotIDs[i] = f.NodeID
	}
	assert.Equal(t, []string{"functions/Orphan"}, gotIDs,
		"only the non-interface-method dead function is flagged")
}

// TestFindSmells_DeadCodeSkipsGeneratedFiles asserts that constructs
// in generated-code files (capnp, protobuf, _generated.go, .gen.go)
// don't surface in dead_code. Generated code intentionally exports
// wide APIs the consumer picks from, so most methods are statically
// "dead" — flagging them buries real findings under noise (256/305
// of mache's pre-filter findings came from one capnp.go file).
func TestFindSmells_DeadCodeSkipsGeneratedFiles(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		CREATE TABLE IF NOT EXISTS node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE IF NOT EXISTS node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;

		-- Defs in generated files — must be skipped.
		INSERT INTO node_defs VALUES
		  ('CapnpFn',     'methods/CapnpFn'),
		  ('PbFn',        'methods/PbFn'),
		  ('GenFn',       'methods/GenFn'),
		  ('DotGenFn',    'methods/DotGenFn');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('methods/CapnpFn',         'methods',           'CapnpFn',  1, 0, '',                     ''),
		  ('methods/CapnpFn/source',  'methods/CapnpFn',   'source',   0, 0, 'a/b/foo.capnp.go',     ''),
		  ('methods/PbFn',            'methods',           'PbFn',     1, 0, '',                     ''),
		  ('methods/PbFn/source',     'methods/PbFn',      'source',   0, 0, 'a/b/foo.pb.go',        ''),
		  ('methods/GenFn',           'methods',           'GenFn',    1, 0, '',                     ''),
		  ('methods/GenFn/source',    'methods/GenFn',     'source',   0, 0, 'a/b/foo_generated.go', ''),
		  ('methods/DotGenFn',        'methods',           'DotGenFn', 1, 0, '',                     ''),
		  ('methods/DotGenFn/source', 'methods/DotGenFn',  'source',   0, 0, 'a/b/foo.gen.go',       '');

		-- Real dead function in normal source file — control: must
		-- still be flagged.
		INSERT INTO node_defs VALUES ('Orphan', 'functions/Orphan');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('functions/Orphan',        'functions',         'Orphan',   1, 0, '',                     ''),
		  ('functions/Orphan/source', 'functions/Orphan',  'source',   0, 0, 'pkg/x.go',             '');
	`)
	require.NoError(t, err)

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{"rule": "dead_code"}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))

	gotIDs := make([]string, len(resp.Findings))
	for i, f := range resp.Findings {
		gotIDs[i] = f.NodeID
		assert.NotContains(t, f.SourceID, ".capnp.go", "capnp generated files must be skipped")
		assert.NotContains(t, f.SourceID, ".pb.go", "protobuf generated files must be skipped")
		assert.NotContains(t, f.SourceID, "_generated.go", "_generated.go suffix must be skipped")
		assert.NotContains(t, f.SourceID, ".gen.go", ".gen.go suffix must be skipped")
	}
	assert.Equal(t, []string{"functions/Orphan"}, gotIDs,
		"only the non-generated dead function is flagged")
}

// TestFindSmells_DeadCodeSkipsOrphanNodes asserts that construct
// dir nodes with no resolvable source_file (neither on the dir
// itself nor on any leaf child) are dropped from dead_code. These
// orphans appear when the engine's processNode persists the dir
// but the file-children loop produces nothing — observed when an
// FCA-inferred Go schema's `{{.scope}}` template runs against a
// non-Go file (e.g. a JS function_declaration matched against the
// inferred Go selector). Flagging the orphan is noise — there's no
// source data to navigate to.
func TestFindSmells_DeadCodeSkipsOrphanNodes(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		-- Orphan: defined, exists, but no leaf children with source_file.
		INSERT INTO node_defs VALUES ('processUser', 'functions/processUser');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('functions/processUser', 'functions', 'processUser', 1, 0, '', '');

		-- Real dead function with leaf children — control: must flag.
		INSERT INTO node_defs VALUES ('Orphan', 'functions/Orphan');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('functions/Orphan',        'functions',         'Orphan', 1, 0, '',         ''),
		  ('functions/Orphan/source', 'functions/Orphan',  'source', 0, 0, 'o.go',     '');
	`)
	require.NoError(t, err)

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{"rule": "dead_code"}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))

	gotIDs := make([]string, len(resp.Findings))
	for i, f := range resp.Findings {
		gotIDs[i] = f.NodeID
	}
	assert.Equal(t, []string{"functions/Orphan"}, gotIDs,
		"only the dead function with leaf-resolvable source_file is flagged; orphan dropped")
}

// TestFindSmells_DeadCodeStripsReceiverPrefixForLeafMatch asserts
// that a method defined as 'Receiver.Method' is treated as alive
// when call-extraction captures the bare 'Method' field_identifier.
// Without the leaf-strip in the alive CTE, every method on a typed
// receiver under the go-schema methods/ branch looked dead because
// the call token never matched 'Receiver.Method' verbatim.
//
// Surfaced by dogfooding: mache.db's go-schema path had 510 dead_code
// findings, 494 of them methods/. After this fix: 32.
func TestFindSmells_DeadCodeStripsReceiverPrefixForLeafMatch(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		CREATE TABLE IF NOT EXISTS node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE IF NOT EXISTS node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;

		-- Method defined under the Receiver.Method shape (go-schema
		-- methods/ branch). The corresponding call site captures
		-- only the field_identifier — bare 'Greet'.
		INSERT INTO node_defs VALUES
		  ('Greeter.Greet',     'pkg/methods/Greeter.Greet'),
		  ('demo.Greeter.Greet','pkg/methods/Greeter.Greet');
		INSERT INTO node_refs VALUES ('Greet', 'pkg/functions/CallSite/source');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/methods/Greeter.Greet',         'pkg/methods', 'Greeter.Greet', 1, 0, '',         ''),
		  ('pkg/methods/Greeter.Greet/source',  'pkg/methods/Greeter.Greet','source',0, 0, 'g.go',     ''),
		  ('pkg/functions/CallSite',            'pkg/functions','CallSite',1, 0, '',                 ''),
		  ('pkg/functions/CallSite/source',     'pkg/functions/CallSite','source', 0, 0, 'c.go',    '');

		-- Truly dead method (no call to bare leaf, no call to
		-- Receiver.Method) — control: must still be flagged.
		INSERT INTO node_defs VALUES
		  ('Greeter.Sing',      'pkg/methods/Greeter.Sing');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/methods/Greeter.Sing',         'pkg/methods', 'Greeter.Sing', 1, 0, '',         ''),
		  ('pkg/methods/Greeter.Sing/source',  'pkg/methods/Greeter.Sing','source',0, 0, 's.go',    '');
	`)
	require.NoError(t, err)

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{"rule": "dead_code"}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))

	gotIDs := make([]string, len(resp.Findings))
	for i, f := range resp.Findings {
		gotIDs[i] = f.NodeID
	}
	assert.Equal(t, []string{"pkg/methods/Greeter.Sing"}, gotIDs,
		"Greeter.Greet is alive (Greet ref matches the leaf-strip); only Greeter.Sing remains dead")
}

// TestFindSmells_DeadCodeSourceFileFallsBackToChildren asserts that
// when the construct dir itself has source_file = ” (the schema
// engine attaches Origin/source_file to leaf children — source,
// ast.json, doc — but not the wrapping dir), the rule resolves the
// source via any child with a non-empty source_file. Without this
// the find_smells GHA workflow couldn't filter findings against the
// PR diff (every finding scoped to ”).
func TestFindSmells_DeadCodeSourceFileFallsBackToChildren(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		CREATE TABLE IF NOT EXISTS node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE IF NOT EXISTS node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;

		INSERT INTO node_defs VALUES ('Orphan', 'functions/Orphan');

		-- Dir node has source_file = '' (real-world FCA-inferred
		-- schema). Leaf children carry the actual file path.
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('functions/Orphan',          'functions',          'Orphan',    1, 0, '',                ''),
		  ('functions/Orphan/source',   'functions/Orphan',   'source',    0, 0, 'orphan.go',       ''),
		  ('functions/Orphan/ast.json', 'functions/Orphan',   'ast.json',  0, 0, 'orphan.go',       ''),
		  ('functions/Orphan/doc',      'functions/Orphan',   'doc',       0, 0, 'orphan.go',       '');
	`)
	require.NoError(t, err)

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{"rule": "dead_code"}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))
	require.Len(t, resp.Findings, 1)
	assert.Equal(t, "functions/Orphan", resp.Findings[0].NodeID)
	assert.Equal(t, "orphan.go", resp.Findings[0].SourceID,
		"source_id falls back to a child's source_file when the construct dir itself has none")
}

// TestFindSmells_DeadCodeSourceFileScopeFilter asserts that the
// child-fallback source_id is the column the source_id query param
// scopes against — i.e. PR-diff filters in the GHA wrapper still
// work after the fallback.
func TestFindSmells_DeadCodeSourceFileScopeFilter(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		CREATE TABLE IF NOT EXISTS node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE IF NOT EXISTS node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;

		-- Two dead constructs in two different files. Same shape:
		-- dir source_file is empty, child source_file holds the path.
		INSERT INTO node_defs VALUES
		  ('OrphanA', 'functions/OrphanA'),
		  ('OrphanB', 'functions/OrphanB');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('functions/OrphanA',        'functions',         'OrphanA', 1, 0, '',          ''),
		  ('functions/OrphanA/source', 'functions/OrphanA', 'source',  0, 0, 'a.go',      ''),
		  ('functions/OrphanB',        'functions',         'OrphanB', 1, 0, '',          ''),
		  ('functions/OrphanB/source', 'functions/OrphanB', 'source',  0, 0, 'b.go',      '');
	`)
	require.NoError(t, err)

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{
		"rule":      "dead_code",
		"source_id": "a.go",
	}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))
	require.Len(t, resp.Findings, 1, "only the OrphanA in a.go matches the source_id filter")
	assert.Equal(t, "functions/OrphanA", resp.Findings[0].NodeID)
	assert.Equal(t, "a.go", resp.Findings[0].SourceID)
}

// TestFindSmells_DeadCodePerNodeAggregation asserts that dead_code
// aggregates by node_id, not by token. A function with multiple
// token aliases (bare + qualified) where ANY token is referenced
// must NOT be flagged dead, and the response never contains
// duplicate node_id entries.
//
// Surfaced by dogfooding mache against itself: 'functions/Unmount'
// has three tokens (Unmount, mount.Unmount, nfsmount.Unmount); only
// the bare 'Unmount' has refs, so the old per-token query flagged
// the construct twice (once per qualified-but-unreferenced token).
func TestFindSmells_DeadCodePerNodeAggregation(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		CREATE TABLE IF NOT EXISTS node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE IF NOT EXISTS node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;

		-- Multi-token live construct: 3 aliases, only bare has refs.
		-- Old per-token query flagged this twice (once per qualified
		-- alias). New per-node query treats it as live (any-token-
		-- referenced means live).
		INSERT INTO node_defs VALUES
		  ('Unmount',          'pkg/Unmount'),
		  ('mount.Unmount',    'pkg/Unmount'),
		  ('nfsmount.Unmount', 'pkg/Unmount');
		INSERT INTO node_refs VALUES ('Unmount', 'pkg/Caller/source');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/Unmount', 'pkg', 'Unmount', 1, 0, 'unmount.go', '');

		-- Truly dead: 2 token aliases, neither referenced.
		INSERT INTO node_defs VALUES
		  ('Orphan',     'pkg/Orphan'),
		  ('pkg.Orphan', 'pkg/Orphan');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/Orphan', 'pkg', 'Orphan', 1, 0, 'orphan.go', '');
	`)
	require.NoError(t, err)

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{"rule": "dead_code"}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))

	gotIDs := make([]string, len(resp.Findings))
	for i, f := range resp.Findings {
		gotIDs[i] = f.NodeID
	}
	assert.Equal(t, []string{"pkg/Orphan"}, gotIDs,
		"only Orphan is flagged; Unmount has a referenced bare token; no duplicate node_ids")
}

// TestFindSmells_DeadCodeSkipsTestingFrameworkPrefixes asserts that
// Test*, Benchmark*, Example*, Fuzz* defs with no static refs are NOT
// flagged. Go's testing framework invokes them via reflection, so they
// never appear in node_refs. The skip list pattern is borrowed from
// untested_function. Surfaced by dogfooding find_smells against mache
// itself.
func TestFindSmells_DeadCodeSkipsTestingFrameworkPrefixes(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		CREATE TABLE IF NOT EXISTS node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE IF NOT EXISTS node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;

		-- Real dead code (control: the rule must still flag this).
		INSERT INTO node_defs VALUES ('OrphanFunc', 'pkg/OrphanFunc');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/OrphanFunc', 'pkg', 'OrphanFunc', 1, 0, 'orphan.go', '');

		-- Each of the 4 testing-framework prefixes — none have refs.
		-- Mix bare and 'pkg.'-qualified tokens to exercise the
		-- substr/instr leaf-extraction in the skip clause.
		INSERT INTO node_defs VALUES
		  ('TestSomething',          'pkg/TestSomething'),
		  ('cmd.TestQualified',      'pkg/TestQualified'),
		  ('BenchmarkSomething',     'pkg/BenchmarkSomething'),
		  ('cmd.BenchmarkQualified', 'pkg/BenchmarkQualified'),
		  ('ExampleSomething',       'pkg/ExampleSomething'),
		  ('FuzzSomething',          'pkg/FuzzSomething');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/TestSomething',      'pkg', 'TestSomething',      1, 0, 'a_test.go',     ''),
		  ('pkg/TestQualified',      'pkg', 'TestQualified',      1, 0, 'q_test.go',     ''),
		  ('pkg/BenchmarkSomething', 'pkg', 'BenchmarkSomething', 1, 0, 'b_test.go',     ''),
		  ('pkg/BenchmarkQualified', 'pkg', 'BenchmarkQualified', 1, 0, 'q_test.go',     ''),
		  ('pkg/ExampleSomething',   'pkg', 'ExampleSomething',   1, 0, 'example_test.go', ''),
		  ('pkg/FuzzSomething',      'pkg', 'FuzzSomething',      1, 0, 'fuzz_test.go',  '');
	`)
	require.NoError(t, err)

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{
		"rule": "dead_code",
	}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))
	require.Equal(t, 1, resp.Total, "only OrphanFunc should be flagged; testing-framework prefixes are skipped")
	assert.Equal(t, "pkg/OrphanFunc", resp.Findings[0].NodeID)
}

// TestFindSmells_DeadCodeSourceIDFilter exercises the per-rule
// ScopeColumn — for dead_code that's `COALESCE(n.source_file, ”)`,
// not the magic-int rule's `lit.source_id`.
func TestFindSmells_DeadCodeSourceIDFilter(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		CREATE TABLE IF NOT EXISTS node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE IF NOT EXISTS node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;

		INSERT INTO node_defs VALUES ('OrphanA', 'pkg/A'), ('OrphanB', 'pkg/B');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/A', 'pkg', 'A', 1, 0, 'a.go', ''),
		  ('pkg/B', 'pkg', 'B', 1, 0, 'b.go', '');
	`)
	require.NoError(t, err)

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{
		"rule":      "dead_code",
		"source_id": "a.go",
	}))
	require.NoError(t, err)

	var resp struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))
	require.Equal(t, 1, resp.Total)
	assert.Equal(t, "a.go", resp.Findings[0].SourceID)
}

// TestFindSmells_CyclomaticComplexity seeds two functions with
// different control-flow counts and asserts the rule returns them
// ranked by metric DESC.
//
// fnA has 3 branches (1 if + 1 for + 1 case) → metric 3.
// fnB has 0 branches → metric 0.
// Both should appear; fnA first.
func TestFindSmells_CyclomaticComplexity(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		INSERT INTO _ast (node_id, source_id, node_kind, start_byte, end_byte, start_row, start_col, end_row, end_col)
		VALUES
		  ('fnA',                'main.go', 'function_declaration', 100, 200, 10, 0, 30, 0),
		  ('fnA/if_a',           'main.go', 'if_statement',         110, 120, 11, 1, 12, 0),
		  ('fnA/for_a',          'main.go', 'for_statement',        130, 150, 13, 1, 16, 0),
		  ('fnA/switch/case_a',  'main.go', 'case_clause',          160, 180, 17, 2, 18, 0),
		  ('fnB',                'main.go', 'function_declaration', 250, 280, 31, 0, 35, 0);
	`)
	require.NoError(t, err)

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{
		"rule": "cyclomatic_complexity",
	}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))

	require.Equal(t, 2, resp.Total)
	assert.Equal(t, "fnA", resp.Findings[0].NodeID, "highest-complexity function ranks first")
	assert.Equal(t, int64(3), resp.Findings[0].Metric, "1 if + 1 for + 1 case = 3")
	assert.Equal(t, "fnB", resp.Findings[1].NodeID)
	assert.Equal(t, int64(0), resp.Findings[1].Metric)
}

// TestFindSmells_CyclomaticOnlyCountsBranchesUnderFunction proves
// the LIKE 'fn.node_id || /%' filter — branches in OTHER functions
// or top-level branches outside any function don't get attributed.
func TestFindSmells_CyclomaticOnlyCountsBranchesUnderFunction(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		INSERT INTO _ast (node_id, source_id, node_kind, start_byte, end_byte, start_row, start_col, end_row, end_col)
		VALUES
		  ('foo',         'main.go', 'function_declaration', 100, 200, 10, 0, 20, 0),
		  ('foo/if_1',    'main.go', 'if_statement',         110, 120, 11, 1, 12, 0),
		  ('bar',         'main.go', 'function_declaration', 300, 400, 31, 0, 40, 0),
		  ('bar/if_1',    'main.go', 'if_statement',         310, 320, 32, 1, 33, 0),
		  ('bar/if_2',    'main.go', 'if_statement',         330, 340, 34, 1, 35, 0),
		  -- A naked top-level if outside any function — must NOT be attributed.
		  ('topif',       'main.go', 'if_statement',         500, 510, 50, 0, 51, 0);
	`)
	require.NoError(t, err)

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{
		"rule": "cyclomatic_complexity",
	}))
	require.NoError(t, err)

	var resp struct {
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))

	got := map[string]int64{}
	for _, f := range resp.Findings {
		got[f.NodeID] = f.Metric
	}
	assert.Equal(t, int64(2), got["bar"], "bar has 2 ifs as descendants")
	assert.Equal(t, int64(1), got["foo"], "foo has 1 if")
	assert.Equal(t, 2, len(got), "topif is not under any function — no extra finding")
}

// TestFindSmells_LongFunction seeds two functions of different sizes
// and asserts the rule flags only the one over the threshold (80 lines).
func TestFindSmells_LongFunction(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		INSERT INTO _ast (node_id, source_id, node_kind, start_byte, end_byte, start_row, start_col, end_row, end_col)
		VALUES
		  -- Long: 100 lines (10 → 110), should fire.
		  ('big_fn',   'main.go', 'function_declaration', 100, 200, 10, 0, 110, 0),
		  -- Just under threshold: exactly 80 lines, should NOT fire (> 80).
		  ('mid_fn',   'main.go', 'method_declaration',   300, 400, 200, 0, 280, 0),
		  -- Tiny: 5 lines.
		  ('small_fn', 'main.go', 'function_declaration', 500, 550, 300, 0, 305, 0);
	`)
	require.NoError(t, err)

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{
		"rule": "long_function",
	}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))
	require.Equal(t, 1, resp.Total, "only big_fn (100 lines) is over the 80-line threshold")
	assert.Equal(t, "big_fn", resp.Findings[0].NodeID)
	assert.Equal(t, int64(100), resp.Findings[0].Metric)
}

// TestFindSmells_UntestedFunction seeds three exported Go-style defs:
// one with a Test counterpart (covered), one without (untested), and
// one whose name is on the skip list (excluded). Plus an unexported
// def to prove the GLOB '[A-Z]' filter holds.
func TestFindSmells_UntestedFunction(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		CREATE TABLE IF NOT EXISTS node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;

		-- Covered: TestFooBar exists, so FooBar is OK.
		-- Paths use 'functions/' category dir — the rule restricts to
		-- that segment to skip methods/, types/, constants/, etc.
		INSERT INTO node_defs VALUES ('FooBar', 'pkg/functions/FooBar');
		INSERT INTO node_defs VALUES ('TestFooBar', 'pkg/functions/TestFooBar');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/functions/FooBar',     'pkg/functions', 'FooBar',     1, 0, 'foo.go',     ''),
		  ('pkg/functions/TestFooBar', 'pkg/functions', 'TestFooBar', 1, 0, 'foo_test.go', '');

		-- Uncovered: no TestOrphan anywhere.
		INSERT INTO node_defs VALUES ('Orphan', 'pkg/functions/Orphan');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/functions/Orphan', 'pkg/functions', 'Orphan', 1, 0, 'orphan.go', '');

		-- Skip list: capitalized but excluded.
		INSERT INTO node_defs VALUES ('String', 'pkg/functions/String');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/functions/String', 'pkg/functions', 'String', 1, 0, 'stringer.go', '');

		-- Unexported (lowercase): must NOT be flagged.
		INSERT INTO node_defs VALUES ('helper', 'pkg/functions/helper');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/functions/helper', 'pkg/functions', 'helper', 1, 0, 'helpers.go', '');

		-- A method (uppercase, no Test counterpart) — must NOT be
		-- flagged because methods/ is outside the rule's scope.
		INSERT INTO node_defs VALUES ('Receiver.Method', 'pkg/methods/Receiver.Method');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/methods/Receiver.Method', 'pkg/methods', 'Receiver.Method', 1, 0, 'receiver.go', '');

		-- A type (uppercase, no Test counterpart) — must NOT be
		-- flagged for the same reason.
		INSERT INTO node_defs VALUES ('Config', 'pkg/types/Config');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/types/Config', 'pkg/types', 'Config', 1, 0, 'config.go', '');
	`)
	require.NoError(t, err)

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
	require.Equal(t, 1, resp.Total, "only Orphan is uncovered + exported + not on skip list + in functions/")
	assert.Equal(t, "pkg/functions/Orphan", resp.Findings[0].NodeID)
	assert.Equal(t, "orphan.go", resp.Findings[0].SourceID)
}

// TestFindSmells_GodFile seeds three source files: one with 15
// definitions (the god file), six with 1 definition each (normal),
// and one with 8 definitions (busy but under the floor). Project
// mean is (15 + 6×1 + 8) / 8 ≈ 3.6, so 3× mean ≈ 10.9. Only the
// god file's 15 defs clears both the 10-def floor AND the 3×-mean
// threshold.
func TestFindSmells_GodFile(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		CREATE TABLE IF NOT EXISTS node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;

		-- God file: 15 distinct defs, all in pkg/god/sprawl.go.
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/god/A','pkg/god','A',1,0,'pkg/god/sprawl.go',''),
		  ('pkg/god/B','pkg/god','B',1,0,'pkg/god/sprawl.go',''),
		  ('pkg/god/C','pkg/god','C',1,0,'pkg/god/sprawl.go',''),
		  ('pkg/god/D','pkg/god','D',1,0,'pkg/god/sprawl.go',''),
		  ('pkg/god/E','pkg/god','E',1,0,'pkg/god/sprawl.go',''),
		  ('pkg/god/F','pkg/god','F',1,0,'pkg/god/sprawl.go',''),
		  ('pkg/god/G','pkg/god','G',1,0,'pkg/god/sprawl.go',''),
		  ('pkg/god/H','pkg/god','H',1,0,'pkg/god/sprawl.go',''),
		  ('pkg/god/I','pkg/god','I',1,0,'pkg/god/sprawl.go',''),
		  ('pkg/god/J','pkg/god','J',1,0,'pkg/god/sprawl.go',''),
		  ('pkg/god/K','pkg/god','K',1,0,'pkg/god/sprawl.go',''),
		  ('pkg/god/L','pkg/god','L',1,0,'pkg/god/sprawl.go',''),
		  ('pkg/god/M','pkg/god','M',1,0,'pkg/god/sprawl.go',''),
		  ('pkg/god/N','pkg/god','N',1,0,'pkg/god/sprawl.go',''),
		  ('pkg/god/O','pkg/god','O',1,0,'pkg/god/sprawl.go','');
		INSERT INTO node_defs VALUES
		  ('A','pkg/god/A'),('B','pkg/god/B'),('C','pkg/god/C'),
		  ('D','pkg/god/D'),('E','pkg/god/E'),('F','pkg/god/F'),
		  ('G','pkg/god/G'),('H','pkg/god/H'),('I','pkg/god/I'),
		  ('J','pkg/god/J'),('K','pkg/god/K'),('L','pkg/god/L'),
		  ('M','pkg/god/M'),('N','pkg/god/N'),('O','pkg/god/O');

		-- Borderline file: 8 defs — over 3×mean (~10.9) gate? No, 8 < 10 floor.
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/big/B1','pkg/big','B1',1,0,'pkg/big/big.go',''),
		  ('pkg/big/B2','pkg/big','B2',1,0,'pkg/big/big.go',''),
		  ('pkg/big/B3','pkg/big','B3',1,0,'pkg/big/big.go',''),
		  ('pkg/big/B4','pkg/big','B4',1,0,'pkg/big/big.go',''),
		  ('pkg/big/B5','pkg/big','B5',1,0,'pkg/big/big.go',''),
		  ('pkg/big/B6','pkg/big','B6',1,0,'pkg/big/big.go',''),
		  ('pkg/big/B7','pkg/big','B7',1,0,'pkg/big/big.go',''),
		  ('pkg/big/B8','pkg/big','B8',1,0,'pkg/big/big.go','');
		INSERT INTO node_defs VALUES
		  ('B1','pkg/big/B1'),('B2','pkg/big/B2'),('B3','pkg/big/B3'),
		  ('B4','pkg/big/B4'),('B5','pkg/big/B5'),('B6','pkg/big/B6'),
		  ('B7','pkg/big/B7'),('B8','pkg/big/B8');

		-- Six normal files, 1 def each — dilutes the project mean.
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/ok/N1','pkg/ok','N1',1,0,'pkg/ok/n1.go',''),
		  ('pkg/ok/N2','pkg/ok','N2',1,0,'pkg/ok/n2.go',''),
		  ('pkg/ok/N3','pkg/ok','N3',1,0,'pkg/ok/n3.go',''),
		  ('pkg/ok/N4','pkg/ok','N4',1,0,'pkg/ok/n4.go',''),
		  ('pkg/ok/N5','pkg/ok','N5',1,0,'pkg/ok/n5.go',''),
		  ('pkg/ok/N6','pkg/ok','N6',1,0,'pkg/ok/n6.go','');
		INSERT INTO node_defs VALUES
		  ('N1','pkg/ok/N1'),('N2','pkg/ok/N2'),('N3','pkg/ok/N3'),
		  ('N4','pkg/ok/N4'),('N5','pkg/ok/N5'),('N6','pkg/ok/N6');
	`)
	require.NoError(t, err)

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{
		"rule": "god_file",
	}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))
	require.Equal(t, 1, resp.Total, "only sprawl.go clears the 10-def floor AND 3×mean threshold")
	assert.Equal(t, "pkg/god/sprawl.go", resp.Findings[0].SourceID)
	assert.Equal(t, int64(15), resp.Findings[0].Metric, "def count is the metric")
}

// TestFindSmells_GodFileFallsBackToChildSource asserts that god_file
// resolves source_file via the construct dir's child leaves when
// the dir itself has none — the FCA-inferred shape used by `mache
// build` without --schema. Before this fix, per_file's GROUP BY
// found zero rows on the FCA path because the dir's source_file
// is ”, so god_file silently returned no findings on mache.db.
func TestFindSmells_GodFileFallsBackToChildSource(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		-- 11 distinct defs all attributed (via leaf source_file) to
		-- the same file. Project mean across the 5 normal files +
		-- this one is (11 + 5) / 6 ≈ 2.7; 3× ≈ 8 — the god file
		-- clears the 10-def floor and the 3× threshold.
		INSERT INTO node_defs VALUES
		  ('A','functions/A'),('B','functions/B'),('C','functions/C'),
		  ('D','functions/D'),('E','functions/E'),('F','functions/F'),
		  ('G','functions/G'),('H','functions/H'),('I','functions/I'),
		  ('J','functions/J'),('K','functions/K');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('functions/A','functions','A',1,0,'',''),
		  ('functions/A/source','functions/A','source',0,0,'pkg/god/sprawl.go',''),
		  ('functions/B','functions','B',1,0,'',''),
		  ('functions/B/source','functions/B','source',0,0,'pkg/god/sprawl.go',''),
		  ('functions/C','functions','C',1,0,'',''),
		  ('functions/C/source','functions/C','source',0,0,'pkg/god/sprawl.go',''),
		  ('functions/D','functions','D',1,0,'',''),
		  ('functions/D/source','functions/D','source',0,0,'pkg/god/sprawl.go',''),
		  ('functions/E','functions','E',1,0,'',''),
		  ('functions/E/source','functions/E','source',0,0,'pkg/god/sprawl.go',''),
		  ('functions/F','functions','F',1,0,'',''),
		  ('functions/F/source','functions/F','source',0,0,'pkg/god/sprawl.go',''),
		  ('functions/G','functions','G',1,0,'',''),
		  ('functions/G/source','functions/G','source',0,0,'pkg/god/sprawl.go',''),
		  ('functions/H','functions','H',1,0,'',''),
		  ('functions/H/source','functions/H','source',0,0,'pkg/god/sprawl.go',''),
		  ('functions/I','functions','I',1,0,'',''),
		  ('functions/I/source','functions/I','source',0,0,'pkg/god/sprawl.go',''),
		  ('functions/J','functions','J',1,0,'',''),
		  ('functions/J/source','functions/J','source',0,0,'pkg/god/sprawl.go',''),
		  ('functions/K','functions','K',1,0,'',''),
		  ('functions/K/source','functions/K','source',0,0,'pkg/god/sprawl.go','');

		-- Five normal files (1 def each) to dilute the project mean.
		INSERT INTO node_defs VALUES
		  ('N1','functions/N1'),('N2','functions/N2'),('N3','functions/N3'),
		  ('N4','functions/N4'),('N5','functions/N5');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('functions/N1','functions','N1',1,0,'',''),
		  ('functions/N1/source','functions/N1','source',0,0,'a.go',''),
		  ('functions/N2','functions','N2',1,0,'',''),
		  ('functions/N2/source','functions/N2','source',0,0,'b.go',''),
		  ('functions/N3','functions','N3',1,0,'',''),
		  ('functions/N3/source','functions/N3','source',0,0,'c.go',''),
		  ('functions/N4','functions','N4',1,0,'',''),
		  ('functions/N4/source','functions/N4','source',0,0,'d.go',''),
		  ('functions/N5','functions','N5',1,0,'',''),
		  ('functions/N5/source','functions/N5','source',0,0,'e.go','');
	`)
	require.NoError(t, err)

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{"rule": "god_file"}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))
	require.Len(t, resp.Findings, 1, "FCA-shape fixture: source_file is on the leaf, not the construct dir")
	assert.Equal(t, "pkg/god/sprawl.go", resp.Findings[0].SourceID)
	assert.Equal(t, int64(11), resp.Findings[0].Metric)
}

// TestFindSmells_GodFileSkipsGeneratedFiles asserts that *.capnp.go,
// *.pb.go, *_generated.go, and *.gen.go aren't flagged even with
// hundreds of definitions — generators produce wide method sets by
// design, not architectural sprawl.
func TestFindSmells_GodFileSkipsGeneratedFiles(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		-- 12 defs in a generated file — would normally trigger
		-- god_file, but must be skipped by the *.capnp.go suffix.
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/methods/A.x','pkg/methods','A.x',1,0,'a.capnp.go',''),
		  ('pkg/methods/A.y','pkg/methods','A.y',1,0,'a.capnp.go',''),
		  ('pkg/methods/B.x','pkg/methods','B.x',1,0,'a.capnp.go',''),
		  ('pkg/methods/B.y','pkg/methods','B.y',1,0,'a.capnp.go',''),
		  ('pkg/methods/C.x','pkg/methods','C.x',1,0,'a.capnp.go',''),
		  ('pkg/methods/C.y','pkg/methods','C.y',1,0,'a.capnp.go',''),
		  ('pkg/methods/D.x','pkg/methods','D.x',1,0,'a.capnp.go',''),
		  ('pkg/methods/D.y','pkg/methods','D.y',1,0,'a.capnp.go',''),
		  ('pkg/methods/E.x','pkg/methods','E.x',1,0,'a.capnp.go',''),
		  ('pkg/methods/E.y','pkg/methods','E.y',1,0,'a.capnp.go',''),
		  ('pkg/methods/F.x','pkg/methods','F.x',1,0,'a.capnp.go',''),
		  ('pkg/methods/F.y','pkg/methods','F.y',1,0,'a.capnp.go','');
		INSERT INTO node_defs VALUES
		  ('A.x','pkg/methods/A.x'),('A.y','pkg/methods/A.y'),
		  ('B.x','pkg/methods/B.x'),('B.y','pkg/methods/B.y'),
		  ('C.x','pkg/methods/C.x'),('C.y','pkg/methods/C.y'),
		  ('D.x','pkg/methods/D.x'),('D.y','pkg/methods/D.y'),
		  ('E.x','pkg/methods/E.x'),('E.y','pkg/methods/E.y'),
		  ('F.x','pkg/methods/F.x'),('F.y','pkg/methods/F.y');

		-- A few normal files to populate the project mean.
		INSERT INTO node_defs VALUES
		  ('N1','functions/N1'),('N2','functions/N2'),('N3','functions/N3');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('functions/N1','functions','N1',1,0,'a.go',''),
		  ('functions/N2','functions','N2',1,0,'b.go',''),
		  ('functions/N3','functions','N3',1,0,'c.go','');
	`)
	require.NoError(t, err)

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{"rule": "god_file"}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))
	assert.Empty(t, resp.Findings, "generated capnp.go file must be skipped despite high def count")
}

// TestFindSmells_FanOutSkew seeds a god-function and several normal
// callers and asserts only the god-function is flagged. Mean fan-out
// is (12 + 6×1) / 7 ≈ 2.57; 3×mean ≈ 7.7, well below the god's 12.
// The 5-call floor would also gate out smaller outliers if they crept
// past the mean check.
func TestFindSmells_FanOutSkew(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		CREATE TABLE IF NOT EXISTS node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;

		-- God-function: 12 distinct callees.
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/god/Dispatcher', 'pkg/god', 'Dispatcher', 1, 0, 'dispatcher.go', '');
		INSERT INTO node_refs VALUES
		  ('A','pkg/god/Dispatcher'),('B','pkg/god/Dispatcher'),('C','pkg/god/Dispatcher'),
		  ('D','pkg/god/Dispatcher'),('E','pkg/god/Dispatcher'),('F','pkg/god/Dispatcher'),
		  ('G','pkg/god/Dispatcher'),('H','pkg/god/Dispatcher'),('I','pkg/god/Dispatcher'),
		  ('J','pkg/god/Dispatcher'),('K','pkg/god/Dispatcher'),('L','pkg/god/Dispatcher');

		-- Six normal callers, 1 callee each — dilutes the project mean
		-- so the god's fan-out exceeds 3× mean. Nodes rows aren't
		-- required since they fail the threshold and never hit the JOIN.
		INSERT INTO node_refs VALUES
		  ('Z1','pkg/ok/N1'),('Z2','pkg/ok/N2'),('Z3','pkg/ok/N3'),
		  ('Z4','pkg/ok/N4'),('Z5','pkg/ok/N5'),('Z6','pkg/ok/N6');

		-- A caller with 6 callees — over the 5-call floor, but still
		-- under 3×mean. Asserts the threshold isn't trivially met.
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/ok/Borderline', 'pkg/ok', 'Borderline', 1, 0, 'borderline.go', '');
		INSERT INTO node_refs VALUES
		  ('M1','pkg/ok/Borderline'),('M2','pkg/ok/Borderline'),
		  ('M3','pkg/ok/Borderline'),('M4','pkg/ok/Borderline'),
		  ('M5','pkg/ok/Borderline'),('M6','pkg/ok/Borderline');
	`)
	require.NoError(t, err)

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{
		"rule": "fan_out_skew",
	}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))
	require.Equal(t, 1, resp.Total, "only Dispatcher exceeds 3×mean and the 5-call floor")
	assert.Equal(t, "pkg/god/Dispatcher", resp.Findings[0].NodeID)
	assert.Equal(t, "dispatcher.go", resp.Findings[0].SourceID)
	assert.Equal(t, int64(12), resp.Findings[0].Metric, "fan-out count is reported as metric")
}

// TestFindSmells_FanOutSkewSkipsTestPrefixes asserts that a Test-
// prefixed construct with high fan-out is NOT flagged. Mirrors how
// mache writes data: caller_id is a source-file node whose parent
// is the construct directory, and ctor.name carries the function
// name. The new skip-list joins through that parent.
//
// Surfaced by dogfooding: mache's own test runners
// (TestArena_AllTools etc) topped fan_out_skew with 48 callees —
// tests are *expected* to call many things; no signal there.
func TestFindSmells_FanOutSkewSkipsTestPrefixes(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		CREATE TABLE IF NOT EXISTS node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;

		-- Construct hierarchy: parent dir → source-file node.
		-- caller_id in node_refs is the source-file id (matches mache's
		-- production write shape), and the parent dir's name is what
		-- the skip-list checks.
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('functions',                        '',          'functions',          1, 0, '',                        ''),
		  ('functions/TestRunner',             'functions', 'TestRunner',         1, 0, 'runner_test.go',          ''),
		  ('functions/TestRunner/source',      'functions/TestRunner', 'source',  0, 0, 'runner_test.go',          ''),
		  ('functions/Dispatcher',             'functions', 'Dispatcher',         1, 0, 'dispatcher.go',           ''),
		  ('functions/Dispatcher/source',      'functions/Dispatcher', 'source',  0, 0, 'dispatcher.go',           '');

		-- TestRunner has 12 distinct callees — would normally trip
		-- fan_out_skew. The new skip-list excludes it on the 'Test'
		-- prefix of the parent ctor.name.
		INSERT INTO node_refs VALUES
		  ('A','functions/TestRunner/source'),('B','functions/TestRunner/source'),('C','functions/TestRunner/source'),
		  ('D','functions/TestRunner/source'),('E','functions/TestRunner/source'),('F','functions/TestRunner/source'),
		  ('G','functions/TestRunner/source'),('H','functions/TestRunner/source'),('I','functions/TestRunner/source'),
		  ('J','functions/TestRunner/source'),('K','functions/TestRunner/source'),('L','functions/TestRunner/source');

		-- Dispatcher also has 12 distinct callees — should be flagged.
		-- (Production code, not test.)
		INSERT INTO node_refs VALUES
		  ('M','functions/Dispatcher/source'),('N','functions/Dispatcher/source'),('O','functions/Dispatcher/source'),
		  ('P','functions/Dispatcher/source'),('Q','functions/Dispatcher/source'),('R','functions/Dispatcher/source'),
		  ('S','functions/Dispatcher/source'),('T','functions/Dispatcher/source'),('U','functions/Dispatcher/source'),
		  ('V','functions/Dispatcher/source'),('W','functions/Dispatcher/source'),('X','functions/Dispatcher/source');

		-- Six tiny callers to dilute the project mean below 3× threshold.
		INSERT INTO node_refs VALUES
		  ('z1','functions/n1'),('z2','functions/n2'),('z3','functions/n3'),
		  ('z4','functions/n4'),('z5','functions/n5'),('z6','functions/n6');
	`)
	require.NoError(t, err)

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{"rule": "fan_out_skew"}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))

	gotIDs := make([]string, len(resp.Findings))
	for i, f := range resp.Findings {
		gotIDs[i] = f.NodeID
	}
	assert.Equal(t, []string{"functions/Dispatcher/source"}, gotIDs,
		"Dispatcher (production code) is flagged; TestRunner (test) is skipped via parent ctor.name LIKE 'Test%'")
}

// TestFindSmells_FanOutSkewSkipsGeneratedFiles asserts that a
// generated-code dispatcher (capnp / pb / *_generated.go / *.gen.go)
// with high fan-out is NOT flagged. Generated dispatchers naturally
// touch many neighbors — flagging them buries real findings under
// noise (capnp.go entries dominated mache's pre-filter top-N).
func TestFindSmells_FanOutSkewSkipsGeneratedFiles(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		CREATE TABLE IF NOT EXISTS node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;

		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('functions',                       '',                       'functions', 1, 0, '',                ''),
		  ('functions/CapnpStruct',           'functions',              'CapnpStruct',1, 0, '',                ''),
		  ('functions/CapnpStruct/source',    'functions/CapnpStruct',  'source',    0, 0, 'foo.capnp.go',    ''),
		  ('functions/Dispatcher',            'functions',              'Dispatcher',1, 0, '',                ''),
		  ('functions/Dispatcher/source',     'functions/Dispatcher',   'source',    0, 0, 'dispatcher.go',   '');

		-- Both have 12 distinct callees. CapnpStruct is in *.capnp.go
		-- so must be skipped; Dispatcher (production) must be flagged.
		INSERT INTO node_refs VALUES
		  ('A','functions/CapnpStruct/source'),('B','functions/CapnpStruct/source'),('C','functions/CapnpStruct/source'),
		  ('D','functions/CapnpStruct/source'),('E','functions/CapnpStruct/source'),('F','functions/CapnpStruct/source'),
		  ('G','functions/CapnpStruct/source'),('H','functions/CapnpStruct/source'),('I','functions/CapnpStruct/source'),
		  ('J','functions/CapnpStruct/source'),('K','functions/CapnpStruct/source'),('L','functions/CapnpStruct/source');

		INSERT INTO node_refs VALUES
		  ('M','functions/Dispatcher/source'),('N','functions/Dispatcher/source'),('O','functions/Dispatcher/source'),
		  ('P','functions/Dispatcher/source'),('Q','functions/Dispatcher/source'),('R','functions/Dispatcher/source'),
		  ('S','functions/Dispatcher/source'),('T','functions/Dispatcher/source'),('U','functions/Dispatcher/source'),
		  ('V','functions/Dispatcher/source'),('W','functions/Dispatcher/source'),('X','functions/Dispatcher/source');

		-- Tiny callers to bring project mean down so 12 trips the threshold.
		INSERT INTO node_refs VALUES
		  ('z1','functions/n1'),('z2','functions/n2'),('z3','functions/n3'),
		  ('z4','functions/n4'),('z5','functions/n5'),('z6','functions/n6');
	`)
	require.NoError(t, err)

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{"rule": "fan_out_skew"}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))

	gotIDs := make([]string, len(resp.Findings))
	for i, f := range resp.Findings {
		gotIDs[i] = f.NodeID
	}
	assert.Equal(t, []string{"functions/Dispatcher/source"}, gotIDs,
		"Dispatcher (production) is flagged; CapnpStruct (generated) is skipped via source_file suffix")
}

// TestFindSmells_UntestedFunctionAcceptsTestCallCoverage asserts
// that a function called from inside any Test*/Benchmark*/Example*/
// Fuzz* construct's source is treated as covered, even without a
// same-name TestFoo counterpart. ReadArenaHeader is called from
// TestArenaFlusher_Coalesce / TestArenaFlusher_FlipBuffer /
// TestCreateArena — that's coverage, even though there's no
// TestReadArenaHeader.
func TestFindSmells_UntestedFunctionAcceptsTestCallCoverage(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		-- Function exercised via call from a TestFoo construct's
		-- source — no same-name TestReadArenaHeader needed.
		INSERT INTO node_defs VALUES ('ReadArenaHeader', 'pkg/functions/ReadArenaHeader');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/functions/ReadArenaHeader',         'pkg/functions',                 'ReadArenaHeader',     1, 0, '',           ''),
		  ('pkg/functions/ReadArenaHeader/source',  'pkg/functions/ReadArenaHeader', 'source',              0, 0, 'arena.go',   ''),
		  ('pkg/functions/TestArena_FlipBuffer',         'pkg/functions',                       'TestArena_FlipBuffer', 1, 0, '',                ''),
		  ('pkg/functions/TestArena_FlipBuffer/source',  'pkg/functions/TestArena_FlipBuffer',  'source',                0, 0, 'arena_test.go',  '');

		INSERT INTO node_refs VALUES ('ReadArenaHeader', 'pkg/functions/TestArena_FlipBuffer/source');

		-- Truly untested helper — control: must still flag.
		INSERT INTO node_defs VALUES ('Orphan', 'pkg/functions/Orphan');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/functions/Orphan',        'pkg/functions',         'Orphan', 1, 0, '',          ''),
		  ('pkg/functions/Orphan/source', 'pkg/functions/Orphan',  'source', 0, 0, 'o.go',      '');
	`)
	require.NoError(t, err)

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{"rule": "untested_function"}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))

	gotIDs := make([]string, len(resp.Findings))
	for i, f := range resp.Findings {
		gotIDs[i] = f.NodeID
	}
	assert.Equal(t, []string{"pkg/functions/Orphan"}, gotIDs,
		"ReadArenaHeader is exercised via call from TestArena_FlipBuffer; only Orphan remains untested")
}

// TestFindSmells_UntestedFunctionSkipsFuzzPrefix asserts that Fuzz*
// functions (themselves fuzz-test entry points) aren't flagged.
// They're covered by the testing framework like Test* and Example*,
// just under a different prefix.
func TestFindSmells_UntestedFunctionSkipsFuzzPrefix(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		INSERT INTO node_defs VALUES
		  ('FuzzParseSelector', 'pkg/functions/FuzzParseSelector'),
		  ('FuzzInferRecords',  'pkg/functions/FuzzInferRecords');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/functions/FuzzParseSelector',        'pkg/functions',                  'FuzzParseSelector', 1, 0, '',         ''),
		  ('pkg/functions/FuzzParseSelector/source', 'pkg/functions/FuzzParseSelector','source',             0, 0, 'p_test.go', ''),
		  ('pkg/functions/FuzzInferRecords',         'pkg/functions',                  'FuzzInferRecords',  1, 0, '',         ''),
		  ('pkg/functions/FuzzInferRecords/source',  'pkg/functions/FuzzInferRecords', 'source',             0, 0, 'i_test.go', '');
	`)
	require.NoError(t, err)

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{"rule": "untested_function"}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))
	assert.Empty(t, resp.Findings, "Fuzz* are themselves test entry points; they shouldn't be flagged as untested")
}

// TestFindSmells_UntestedFunctionAcceptsTestTypePrefix asserts the
// New<Type> → Test<Type>* alternate counterpart match. Go test code
// for constructors typically lives under TestType_<Method> — not
// under TestNewType — so flagging NewType as 'untested' when the
// type IS tested is a noise FP.
func TestFindSmells_UntestedFunctionAcceptsTestTypePrefix(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		CREATE TABLE IF NOT EXISTS node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;

		-- Constructor with companion TestStore_Foo coverage — must NOT be flagged.
		INSERT INTO node_defs VALUES
		  ('NewStore',          'pkg/functions/NewStore'),
		  ('TestStore_AddItem', 'pkg/functions/TestStore_AddItem');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/functions/NewStore',                'pkg/functions', 'NewStore', 1, 0, '',                 ''),
		  ('pkg/functions/NewStore/source',         'pkg/functions/NewStore', 'source', 0, 0, 's.go',         ''),
		  ('pkg/functions/TestStore_AddItem',       'pkg/functions', 'TestStore_AddItem', 1, 0, '',         ''),
		  ('pkg/functions/TestStore_AddItem/source','pkg/functions/TestStore_AddItem','source',0, 0, 's_t.go',     '');

		-- Truly untested constructor — control: must still flag.
		INSERT INTO node_defs VALUES ('NewOrphan', 'pkg/functions/NewOrphan');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/functions/NewOrphan',        'pkg/functions',           'NewOrphan', 1, 0, '',          ''),
		  ('pkg/functions/NewOrphan/source', 'pkg/functions/NewOrphan', 'source',    0, 0, 'o.go',     '');

		-- Untested non-constructor — control: must still flag, since
		-- the New-prefix lookup specifically requires 'New' prefix.
		INSERT INTO node_defs VALUES ('Helper', 'pkg/functions/Helper');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/functions/Helper',        'pkg/functions',         'Helper', 1, 0, '',         ''),
		  ('pkg/functions/Helper/source', 'pkg/functions/Helper',  'source', 0, 0, 'h.go',    '');

		-- Tested non-constructor (TestHelper exact match) — must NOT flag.
		INSERT INTO node_defs VALUES
		  ('Validate',     'pkg/functions/Validate'),
		  ('TestValidate', 'pkg/functions/TestValidate');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/functions/Validate',         'pkg/functions',           'Validate', 1, 0, '',          ''),
		  ('pkg/functions/Validate/source',  'pkg/functions/Validate',  'source',   0, 0, 'v.go',    ''),
		  ('pkg/functions/TestValidate',     'pkg/functions',           'TestValidate', 1, 0, '',     ''),
		  ('pkg/functions/TestValidate/source','pkg/functions/TestValidate','source',0, 0, 'v_t.go',  '');
	`)
	require.NoError(t, err)

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{"rule": "untested_function"}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))

	gotIDs := make([]string, len(resp.Findings))
	for i, f := range resp.Findings {
		gotIDs[i] = f.NodeID
	}
	assert.ElementsMatch(t,
		[]string{"pkg/functions/NewOrphan", "pkg/functions/Helper"},
		gotIDs,
		"NewStore is satisfied by TestStore_*; Validate is satisfied by exact TestValidate; only NewOrphan and Helper remain",
	)
}

// TestFindSmells_UntestedFunctionSkipsGeneratedFiles asserts that
// exported funcs in generated-code files don't get flagged for
// missing TestFoo coverage — generated APIs aren't expected to have
// project-side tests.
func TestFindSmells_UntestedFunctionSkipsGeneratedFiles(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		CREATE TABLE IF NOT EXISTS node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;

		-- Exported funcs in generated files — must be skipped.
		INSERT INTO node_defs VALUES
		  ('NewMessage',  'functions/NewMessage'),
		  ('NewBlob',     'functions/NewBlob');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('functions/NewMessage',         'functions',           'NewMessage', 1, 0, '',              ''),
		  ('functions/NewMessage/source',  'functions/NewMessage','source',     0, 0, 'm.capnp.go',    ''),
		  ('functions/NewBlob',            'functions',           'NewBlob',    1, 0, '',              ''),
		  ('functions/NewBlob/source',     'functions/NewBlob',   'source',     0, 0, 'b.pb.go',       '');

		-- Real exported func in normal source — control: must be flagged
		-- (no TestPublishFunction in node_defs).
		INSERT INTO node_defs VALUES ('PublishFunction', 'functions/PublishFunction');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('functions/PublishFunction',        'functions',                 'PublishFunction', 1, 0, '',         ''),
		  ('functions/PublishFunction/source', 'functions/PublishFunction', 'source',          0, 0, 'pub.go',   '');
	`)
	require.NoError(t, err)

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{"rule": "untested_function"}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))

	gotIDs := make([]string, len(resp.Findings))
	for i, f := range resp.Findings {
		gotIDs[i] = f.NodeID
	}
	assert.Equal(t, []string{"functions/PublishFunction"}, gotIDs,
		"only the non-generated exported func without a TestFoo counterpart is flagged")
}

// TestFindSmells_DuplicateDefinitions seeds three groups: a duplicated
// helper (two defs, two source files — flagged twice), an interface
// method on the skip list (two defs — excluded), and a unique symbol
// (one def — excluded).
func TestFindSmells_DuplicateDefinitions(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		CREATE TABLE IF NOT EXISTS node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;

		-- Duplicated helper — both should appear in findings.
		INSERT INTO node_defs VALUES ('Helper', 'pkg/a/Helper'), ('Helper', 'pkg/b/Helper');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/a/Helper', 'pkg/a', 'Helper', 1, 0, 'a/helper.go', ''),
		  ('pkg/b/Helper', 'pkg/b', 'Helper', 1, 0, 'b/helper.go', '');

		-- Interface method — duplication is expected, must be excluded.
		INSERT INTO node_defs VALUES ('String', 'pkg/a/String'), ('String', 'pkg/b/String');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/a/String', 'pkg/a', 'String', 1, 0, 'a/stringer.go', ''),
		  ('pkg/b/String', 'pkg/b', 'String', 1, 0, 'b/stringer.go', '');

		-- Unique symbol — must NOT be flagged.
		INSERT INTO node_defs VALUES ('Solo', 'pkg/a/Solo');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/a/Solo', 'pkg/a', 'Solo', 1, 0, 'a/solo.go', '');
	`)
	require.NoError(t, err)

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{
		"rule": "duplicate_definitions",
	}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))
	require.Equal(t, 2, resp.Total, "both Helper defs flagged, String skipped, Solo not duplicated")

	gotIDs := []string{resp.Findings[0].NodeID, resp.Findings[1].NodeID}
	assert.ElementsMatch(t, []string{"pkg/a/Helper", "pkg/b/Helper"}, gotIDs)
	for _, f := range resp.Findings {
		assert.Equal(t, int64(2), f.Metric, "duplicate count reported as metric")
	}
}

// TestFindSmells_DuplicateDefinitionsSkipsImports asserts that nodes
// under '/imports/' don't surface as duplicate definitions, even
// when the same import path appears across many packages. Imports
// are external references, not definitions.
//
// Surfaced by dogfooding: 559 → 200 findings on mache.db after
// excluding imports/.
func TestFindSmells_DuplicateDefinitionsSkipsImports(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		CREATE TABLE IF NOT EXISTS node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;

		-- Same import path in three packages — these should NOT
		-- surface as a duplicate-defs finding.
		INSERT INTO node_defs VALUES
		  ('"fmt"', 'pkg/a/imports/"fmt"'),
		  ('"fmt"', 'pkg/b/imports/"fmt"'),
		  ('"fmt"', 'pkg/c/imports/"fmt"');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/a/imports/"fmt"', 'pkg/a/imports', '"fmt"', 1, 0, 'a/main.go', ''),
		  ('pkg/b/imports/"fmt"', 'pkg/b/imports', '"fmt"', 1, 0, 'b/main.go', ''),
		  ('pkg/c/imports/"fmt"', 'pkg/c/imports', '"fmt"', 1, 0, 'c/main.go', '');

		-- Real duplicate (functions with the same name in two
		-- packages) — control: this should still be flagged.
		INSERT INTO node_defs VALUES
		  ('Helper', 'pkg/a/functions/Helper'),
		  ('Helper', 'pkg/b/functions/Helper');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/a/functions/Helper', 'pkg/a/functions', 'Helper', 1, 0, 'a/helper.go', ''),
		  ('pkg/b/functions/Helper', 'pkg/b/functions', 'Helper', 1, 0, 'b/helper.go', '');
	`)
	require.NoError(t, err)

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{"rule": "duplicate_definitions"}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))

	gotIDs := make([]string, len(resp.Findings))
	for i, f := range resp.Findings {
		gotIDs[i] = f.NodeID
	}
	for _, id := range gotIDs {
		assert.NotContains(t, id, "/imports/",
			"imports/ nodes must not appear in duplicate_definitions")
	}
	assert.ElementsMatch(t,
		[]string{"pkg/a/functions/Helper", "pkg/b/functions/Helper"},
		gotIDs,
		"only the real Helper duplicate is flagged",
	)
}

// TestFindSmells_DuplicateDefinitionsSkipsGeneratedFiles asserts
// that capnp / protobuf / *_generated.go / *.gen.go method sets
// don't surface in duplicate_definitions. Generated types share a
// fixed method set (DecodeFromPtr, EncodeAsPtr, IsValid, Message,
// Segment, ToPtr) — flagging that as 'duplication' buries real
// findings under generator output.
func TestFindSmells_DuplicateDefinitionsSkipsGeneratedFiles(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		CREATE TABLE IF NOT EXISTS node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;

		-- Generated types sharing method 'IsValid' — must be skipped.
		INSERT INTO node_defs VALUES
		  ('TypeA.IsValid', 'pkg/methods/TypeA.IsValid'),
		  ('TypeB.IsValid', 'pkg/methods/TypeB.IsValid'),
		  ('IsValid',       'pkg/methods/TypeA.IsValid'),
		  ('IsValid',       'pkg/methods/TypeB.IsValid');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/methods/TypeA.IsValid',         'pkg/methods', 'TypeA.IsValid', 1, 0, '',                ''),
		  ('pkg/methods/TypeA.IsValid/source',  'pkg/methods/TypeA.IsValid', 'source', 0, 0, 'a.capnp.go', ''),
		  ('pkg/methods/TypeB.IsValid',         'pkg/methods', 'TypeB.IsValid', 1, 0, '',                ''),
		  ('pkg/methods/TypeB.IsValid/source',  'pkg/methods/TypeB.IsValid', 'source', 0, 0, 'b.capnp.go', '');

		-- Real duplicate as a control: a same-name method across
		-- two PRODUCTION (non-generated) types — must still flag.
		INSERT INTO node_defs VALUES
		  ('Helper', 'pkg/a/functions/Helper'),
		  ('Helper', 'pkg/b/functions/Helper');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/a/functions/Helper',        'pkg/a/functions', 'Helper', 1, 0, '',         ''),
		  ('pkg/a/functions/Helper/source', 'pkg/a/functions/Helper', 'source', 0, 0, 'a.go',     ''),
		  ('pkg/b/functions/Helper',        'pkg/b/functions', 'Helper', 1, 0, '',         ''),
		  ('pkg/b/functions/Helper/source', 'pkg/b/functions/Helper', 'source', 0, 0, 'b.go',     '');
	`)
	require.NoError(t, err)

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{"rule": "duplicate_definitions"}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))

	gotIDs := make([]string, len(resp.Findings))
	for i, f := range resp.Findings {
		gotIDs[i] = f.NodeID
	}
	for _, f := range resp.Findings {
		assert.NotContains(t, f.SourceID, ".capnp.go", "capnp generated files must be skipped")
	}
	assert.ElementsMatch(t,
		[]string{"pkg/a/functions/Helper", "pkg/b/functions/Helper"},
		gotIDs,
		"only the production-source duplicate is flagged",
	)
}

// TestFindSmells_DuplicateDefinitionsSkipsNonCallableCategories
// asserts that types/, constants/, variables/ duplicates don't
// surface — common short names like 'content', 'src', 'name'
// legitimately duplicate as local variables across functions and
// aren't meaningful "duplicate definitions". Surfaced by PR #243's
// GHA comment showing 9 variable-shape duplicate findings.
func TestFindSmells_DuplicateDefinitionsSkipsNonCallableCategories(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		CREATE TABLE IF NOT EXISTS node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;

		-- Variable / constant / type duplicates — must NOT surface.
		INSERT INTO node_defs VALUES
		  ('content', 'pkg/variables/content'),
		  ('content', 'pkg/other/variables/content'),
		  ('MaxSize', 'pkg/constants/MaxSize'),
		  ('MaxSize', 'pkg/other/constants/MaxSize'),
		  ('Result',  'pkg/types/Result'),
		  ('Result',  'pkg/other/types/Result');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/variables/content',       'pkg/variables', 'content', 1, 0, 'a.go', ''),
		  ('pkg/other/variables/content', 'pkg/other/variables', 'content', 1, 0, 'b.go', ''),
		  ('pkg/constants/MaxSize',       'pkg/constants', 'MaxSize', 1, 0, 'c.go', ''),
		  ('pkg/other/constants/MaxSize', 'pkg/other/constants', 'MaxSize', 1, 0, 'd.go', ''),
		  ('pkg/types/Result',            'pkg/types',     'Result',  1, 0, 'e.go', ''),
		  ('pkg/other/types/Result',      'pkg/other/types', 'Result', 1, 0, 'f.go', '');

		-- Real function duplicate — control: must still flag.
		INSERT INTO node_defs VALUES
		  ('Helper', 'pkg/a/functions/Helper'),
		  ('Helper', 'pkg/b/functions/Helper');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/a/functions/Helper', 'pkg/a/functions', 'Helper', 1, 0, 'a/h.go', ''),
		  ('pkg/b/functions/Helper', 'pkg/b/functions', 'Helper', 1, 0, 'b/h.go', '');
	`)
	require.NoError(t, err)

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{"rule": "duplicate_definitions"}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))

	gotIDs := make([]string, len(resp.Findings))
	for i, f := range resp.Findings {
		gotIDs[i] = f.NodeID
	}
	for _, id := range gotIDs {
		assert.NotContains(t, id, "/types/", "types/ must be skipped")
		assert.NotContains(t, id, "/constants/", "constants/ must be skipped")
		assert.NotContains(t, id, "/variables/", "variables/ must be skipped")
	}
	assert.ElementsMatch(t,
		[]string{"pkg/a/functions/Helper", "pkg/b/functions/Helper"},
		gotIDs,
		"only the real function duplicate is flagged",
	)
}

// TestFindSmells_DuplicateDefinitionsSkipsQualifiedInit asserts that
// the skip list strips package qualifiers before comparing against
// the leaf-token list. mache build emits both bare ('init') and
// qualified shapes ('cmd.init', 'lang.init') into node_defs; without
// qualifier-stripping the qualified shapes leak through and every
// Go package's init() function shows up as a duplicate definition.
func TestFindSmells_DuplicateDefinitionsSkipsQualifiedInit(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		CREATE TABLE IF NOT EXISTS node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;

		-- Three init() functions across packages — Go expects one
		-- per package, so neither bare 'init' nor 'cmd.init' /
		-- 'lang.init' should surface as a duplicate.
		INSERT INTO node_defs VALUES
		  ('init',      'cmd/functions/init'),
		  ('cmd.init',  'cmd/functions/init'),
		  ('init',      'lang/functions/init'),
		  ('lang.init', 'lang/functions/init'),
		  ('init',      'ingest/functions/init'),
		  ('ingest.init','ingest/functions/init');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('cmd/functions/init',    'cmd/functions',    'init', 1, 0, 'cmd/x.go',    ''),
		  ('lang/functions/init',   'lang/functions',   'init', 1, 0, 'lang/x.go',   ''),
		  ('ingest/functions/init', 'ingest/functions', 'init', 1, 0, 'ingest/x.go', '');

		-- Real duplicate as a control: a same-named helper across
		-- two packages with no qualifier shape that hits the skip
		-- list. This must still be flagged.
		INSERT INTO node_defs VALUES
		  ('myHelper', 'pkg/a/functions/myHelper'),
		  ('myHelper', 'pkg/b/functions/myHelper');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/a/functions/myHelper', 'pkg/a/functions', 'myHelper', 1, 0, 'a/h.go', ''),
		  ('pkg/b/functions/myHelper', 'pkg/b/functions', 'myHelper', 1, 0, 'b/h.go', '');
	`)
	require.NoError(t, err)

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{"rule": "duplicate_definitions"}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))

	gotIDs := make([]string, len(resp.Findings))
	for i, f := range resp.Findings {
		gotIDs[i] = f.NodeID
	}
	for _, id := range gotIDs {
		assert.NotContains(t, id, "/init",
			"init() nodes must be skipped regardless of qualifier shape")
	}
	assert.ElementsMatch(t,
		[]string{"pkg/a/functions/myHelper", "pkg/b/functions/myHelper"},
		gotIDs,
		"only the real myHelper duplicate is flagged",
	)
}

// TestFindSmells_LongFile flags _ast source_file rows over 1500 lines.
func TestFindSmells_LongFile(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		INSERT INTO _ast (node_id, source_id, node_kind, start_byte, end_byte, start_row, start_col, end_row, end_col)
		VALUES
		  ('huge_file_root', 'huge.go',  'source_file', 0, 999999, 0, 0, 2000, 0),
		  ('small_file_root','small.go', 'source_file', 0, 1234,   0, 0, 50,   0);
	`)
	require.NoError(t, err)

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{
		"rule": "long_file",
	}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))
	require.Equal(t, 1, resp.Total, "only huge.go is over 1500 lines")
	assert.Equal(t, "huge.go", resp.Findings[0].SourceID)
	assert.Equal(t, int64(2000), resp.Findings[0].Metric)
}

func TestFindSmells_LimitCaps(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{
		"rule":  "magic_int_in_comparison",
		"limit": float64(1),
	}))
	require.NoError(t, err)

	var resp struct {
		Total int `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))
	assert.Equal(t, 1, resp.Total)
}

// TestFindSmells_MinMetricFilters seeds a long_file rule run with
// three source files of escalating size and asserts the min_metric
// arg gates findings on the metric column.
func TestFindSmells_MinMetricFilters(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		INSERT INTO _ast (node_id, source_id, node_kind, start_byte, end_byte, start_row, start_col, end_row, end_col)
		VALUES
		  ('a_root', 'a.go', 'source_file', 0, 100, 0, 0, 1600, 0),
		  ('b_root', 'b.go', 'source_file', 0, 100, 0, 0, 2000, 0),
		  ('c_root', 'c.go', 'source_file', 0, 100, 0, 0, 5000, 0);
	`)
	require.NoError(t, err)

	handler := makeFindSmellsHandler(tg)

	// No threshold: all three files (>1500 lines) come back.
	res, err := handler(context.Background(), makeRequest(map[string]any{"rule": "long_file"}))
	require.NoError(t, err)
	require.False(t, res.IsError)
	var unfiltered struct {
		Total int `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &unfiltered))
	require.Equal(t, 3, unfiltered.Total, "all three files exceed the rule's 1500-line floor")

	// min_metric=2500 should keep only c.go (5000 lines).
	res, err = handler(context.Background(), makeRequest(map[string]any{
		"rule":       "long_file",
		"min_metric": float64(2500),
	}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	var filtered struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &filtered))
	require.Equal(t, 1, filtered.Total, "only c.go passes the 2500-line cutoff")
	assert.Equal(t, "c.go", filtered.Findings[0].SourceID)
	assert.Equal(t, int64(5000), filtered.Findings[0].Metric)
}
