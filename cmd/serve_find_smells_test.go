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
	// any backend that populates the cross-ref tables.
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
		CREATE TABLE node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;

		-- Covered: TestFooBar exists, so FooBar is OK.
		INSERT INTO node_defs VALUES ('FooBar', 'pkg/funcs/FooBar');
		INSERT INTO node_defs VALUES ('TestFooBar', 'pkg/funcs/TestFooBar');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/funcs/FooBar',     'pkg/funcs', 'FooBar',     1, 0, 'foo.go',     ''),
		  ('pkg/funcs/TestFooBar', 'pkg/funcs', 'TestFooBar', 1, 0, 'foo_test.go', '');

		-- Uncovered: no TestOrphan anywhere.
		INSERT INTO node_defs VALUES ('Orphan', 'pkg/funcs/Orphan');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/funcs/Orphan', 'pkg/funcs', 'Orphan', 1, 0, 'orphan.go', '');

		-- Skip list: capitalized but excluded.
		INSERT INTO node_defs VALUES ('String', 'pkg/funcs/String');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/funcs/String', 'pkg/funcs', 'String', 1, 0, 'stringer.go', '');

		-- Unexported (lowercase): must NOT be flagged.
		INSERT INTO node_defs VALUES ('helper', 'pkg/funcs/helper');
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file, record) VALUES
		  ('pkg/funcs/helper', 'pkg/funcs', 'helper', 1, 0, 'helpers.go', '');
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
	require.Equal(t, 1, resp.Total, "only Orphan is uncovered + exported + not on skip list")
	assert.Equal(t, "pkg/funcs/Orphan", resp.Findings[0].NodeID)
	assert.Equal(t, "orphan.go", resp.Findings[0].SourceID)
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
		CREATE TABLE node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;

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

// TestFindSmells_DuplicateDefinitions seeds three groups: a duplicated
// helper (two defs, two source files — flagged twice), an interface
// method on the skip list (two defs — excluded), and a unique symbol
// (one def — excluded).
func TestFindSmells_DuplicateDefinitions(t *testing.T) {
	tg := seedSmellAST(t)
	defer func() { _ = tg.db.Close() }()

	_, err := tg.db.Exec(`
		CREATE TABLE node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;

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
