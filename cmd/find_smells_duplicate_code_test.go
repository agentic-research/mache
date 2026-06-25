package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"

	"github.com/agentic-research/mache/internal/graph"
)

// seedDuplicateCodeAST builds an _ast fixture with three Go functions:
//
//   - fnA (a.go) and fnB (b.go): byte-for-byte different source, but
//     STRUCTURALLY identical — same node_kind sequence at the same
//     relative depths. These are a Type-2 clone (differ only in
//     identifiers/literals, which leaf-erased structural hashing
//     ignores). Both must be flagged.
//   - fnC (c.go): a different shape (a for-loop instead of a bare
//     return) — a unique structure that must NOT be flagged.
//
// The rule's signature is the ordered (relative_depth ':' node_kind)
// sequence of every node in the function subtree, so fnA and fnB
// collide and fnC does not.
func seedDuplicateCodeAST(t *testing.T) *smellTestGraph {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "dup.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)

	_, err = db.Exec(`
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

	for _, s := range []string{"a.go", "b.go", "c.go"} {
		_, err = db.Exec("INSERT INTO _source VALUES (?, 'go', ?)", s, []byte("package main\n"))
		require.NoError(t, err)
	}

	type ast struct {
		id, src, kind string
		s             int
	}
	rows := []ast{
		// fnA — structure S1 (function/params/block/return/identifier).
		{"a", "a.go", "function_declaration", 0},
		{"a/params", "a.go", "parameter_list", 10},
		{"a/body", "a.go", "block", 20},
		{"a/body/ret", "a.go", "return_statement", 25},
		{"a/body/ret/id", "a.go", "identifier", 30},
		// fnB — same structure S1, different file + byte offsets.
		{"b", "b.go", "function_declaration", 0},
		{"b/params", "b.go", "parameter_list", 10},
		{"b/body", "b.go", "block", 20},
		{"b/body/ret", "b.go", "return_statement", 25},
		{"b/body/ret/id", "b.go", "identifier", 30},
		// fnC — different structure (for-loop, no params), unique.
		{"c", "c.go", "function_declaration", 0},
		{"c/body", "c.go", "block", 20},
		{"c/body/forl", "c.go", "for_statement", 25},
	}
	for _, a := range rows {
		_, err = db.Exec(
			"INSERT INTO _ast (node_id, source_id, node_kind, start_byte, end_byte, start_row, start_col, end_row, end_col) VALUES (?, ?, ?, ?, ?, 0, 0, 0, 0)",
			a.id, a.src, a.kind, a.s, a.s+1,
		)
		require.NoError(t, err)
	}

	return &smellTestGraph{MemoryStore: graph.NewMemoryStore(), db: db}
}

// TestFindSmells_DuplicateCode pins the deterministic clone-detection
// rule: two structurally-identical function subtrees (in different
// files, with different identifiers) are flagged as duplicate_code;
// a structurally-distinct function is not. Detection is leaf-erased
// AST structural hashing — pure SQL over the _ast table, no heuristic
// token matching. min_metric=0 bypasses the default size floor so the
// small fixture functions surface.
func TestFindSmells_DuplicateCode(t *testing.T) {
	tg := seedDuplicateCodeAST(t)
	defer func() { _ = tg.db.Close() }()

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{
		"rule":       "duplicate_code",
		"min_metric": 0,
	}))
	require.NoError(t, err)
	require.False(t, res.IsError, "duplicate_code should run cleanly: %s", resultText(t, res))

	var resp struct {
		Rule     string         `json:"rule"`
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))

	assert.Equal(t, "duplicate_code", resp.Rule)
	require.Equal(t, 2, resp.Total, "exactly the fnA/fnB clone pair; fnC is structurally unique")

	got := map[string]bool{}
	for _, f := range resp.Findings {
		got[f.NodeID] = true
		assert.Equal(t, int64(5), f.Metric, "metric is the duplicated subtree node count (5)")
	}
	assert.True(t, got["a"], "fnA must be flagged")
	assert.True(t, got["b"], "fnB must be flagged")
	assert.False(t, got["c"], "fnC (unique structure) must NOT be flagged")
}

// TestFindSmells_DuplicateCode_ExcludesGenerated pins that generated
// code is excluded from clone candidates entirely (matching every other
// structural rule — capnp/pb/*_generated/*.gen produce wide identical
// method sets by design). A generated function structurally identical to
// two production functions must NOT itself be flagged, and must NOT keep
// the production pair from being detected on their own.
func TestFindSmells_DuplicateCode_ExcludesGenerated(t *testing.T) {
	tg := seedDuplicateCodeAST(t)
	defer func() { _ = tg.db.Close() }()

	// Add a third structural twin of fnA/fnB, but in a generated file.
	_, err := tg.db.Exec("INSERT INTO _source VALUES ('z.gen.go', 'go', ?)", []byte("package main\n"))
	require.NoError(t, err)
	// Same byte layout as fnA/fnB so z is a genuine structural twin —
	// distinct start_bytes (10/20/25/30) reproduce the params-before-body
	// document order; otherwise the ORDER BY tiebreak would reorder the
	// sequence and z wouldn't collide (a vacuity trap).
	for _, r := range []struct {
		id, kind string
		b        int
	}{
		{"z", "function_declaration", 0},
		{"z/params", "parameter_list", 10},
		{"z/body", "block", 20},
		{"z/body/ret", "return_statement", 25},
		{"z/body/ret/id", "identifier", 30},
	} {
		_, err = tg.db.Exec(
			"INSERT INTO _ast (node_id, source_id, node_kind, start_byte, end_byte, start_row, start_col, end_row, end_col) VALUES (?, 'z.gen.go', ?, ?, ?, 0, 0, 0, 0)",
			r.id, r.kind, r.b, r.b+1,
		)
		require.NoError(t, err)
	}

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{
		"rule":       "duplicate_code",
		"min_metric": 0,
	}))
	require.NoError(t, err)
	require.False(t, res.IsError, "duplicate_code should run cleanly: %s", resultText(t, res))

	var resp struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))

	got := map[string]bool{}
	for _, f := range resp.Findings {
		got[f.NodeID] = true
	}
	assert.False(t, got["z"], "generated-file clone must NOT be flagged")
	assert.True(t, got["a"], "production fnA still detected via its fnB pair")
	assert.True(t, got["b"], "production fnB still detected via its fnA pair")
	assert.Equal(t, 2, resp.Total, "only the two production clones; generated twin excluded")
}

// TestFindSmells_DuplicateCode_DefaultFloorDropsTrivial pins that the
// rule's DefaultMinMetric (24-node floor) is load-bearing: the 5-node
// fixture clones are real structural duplicates, but below the default
// floor, so running WITHOUT min_metric returns nothing. This is what
// keeps one-line accessors and matching stubs out of the default view.
func TestFindSmells_DuplicateCode_DefaultFloorDropsTrivial(t *testing.T) {
	tg := seedDuplicateCodeAST(t)
	defer func() { _ = tg.db.Close() }()

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{
		"rule": "duplicate_code", // no min_metric → DefaultMinMetric=24 applies
	}))
	require.NoError(t, err)
	require.False(t, res.IsError, "duplicate_code should run cleanly: %s", resultText(t, res))

	var resp struct {
		Total int `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))
	assert.Equal(t, 0, resp.Total, "5-node clones are below the 24-node default floor")
}

// TestFindSmells_DuplicateCode_SourceIDScopes pins that detection is
// GLOBAL but the returned findings are scoped: the fnA/fnB clone group
// spans a.go and b.go, so scoping to a.go must still recognize fnA as a
// clone (its pair lives in b.go) yet return only the a.go instance.
func TestFindSmells_DuplicateCode_SourceIDScopes(t *testing.T) {
	tg := seedDuplicateCodeAST(t)
	defer func() { _ = tg.db.Close() }()

	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{
		"rule":       "duplicate_code",
		"min_metric": 0,
		"source_id":  "a.go",
	}))
	require.NoError(t, err)
	require.False(t, res.IsError, "duplicate_code should run cleanly: %s", resultText(t, res))

	var resp struct {
		Total    int            `json:"total"`
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))
	require.Equal(t, 1, resp.Total, "only the a.go instance is returned, but it was detected via its b.go pair")
	assert.Equal(t, "a", resp.Findings[0].NodeID)
	assert.Equal(t, "a.go", resp.Findings[0].SourceID)
}
