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
		} `json:"rules"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))
	assert.NotEmpty(t, resp.Help)

	gotIDs := make([]string, 0, len(resp.Rules))
	for _, r := range resp.Rules {
		gotIDs = append(gotIDs, r.ID)
	}
	assert.Contains(t, gotIDs, "magic_int_in_comparison")
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
		CREATE TABLE node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;

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

// TestFindSmells_DeadCodeSourceIDFilter exercises the per-rule
// ScopeColumn — for dead_code that's `COALESCE(n.source_file, ”)`,
// not the magic-int rule's `lit.source_id`.
func TestFindSmells_DeadCodeSourceIDFilter(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		CREATE TABLE node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;

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
