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
)

// mache-caaae9 — after LLO v0.7.0 (qualified tokens + Python/JS/TS extraction +
// populated source_file), two false-positive classes remain because the rules
// still key on the mache Go-schema node_id taxonomy:
//
//   - duplicate_definitions: leyline DUAL-REGISTERS methods (qualified `Foo.new`
//     AND bare `new`), and the bare aliases still collide across types. #505
//     excludes Rust/Go method containers (impl_item / function_signature_item /
//     method_declaration) but NOT Python (`class_definition` → `function_
//     definition`) or JS/TS (`class_body` → `method_definition`).
//   - dead_code: now RUNS on leyline dbs (source_file populated) and false-
//     positives on TYPES — the `types/`/`constants/` exclusions don't match
//     `struct_item` / `trait_item` / `class_definition` / `class_declaration`.
//
// Node_id shapes below are the real ones emitted by `leyline parse` (v0.7.0).

func newCrossLangGraph(t *testing.T, ddl string) *smellTestGraph {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "xlang.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`
		CREATE TABLE nodes (
			id TEXT PRIMARY KEY, parent_id TEXT, name TEXT NOT NULL,
			kind INTEGER NOT NULL, size INTEGER, mtime INTEGER NOT NULL,
			record_id TEXT, record JSON, source_file TEXT
		);
		CREATE TABLE node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
	` + ddl)
	require.NoError(t, err)
	return &smellTestGraph{db: db}
}

func findingsFor(t *testing.T, tg *smellTestGraph, rule string) []smellFinding {
	t.Helper()
	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{"rule": rule, "limit": float64(1000)}))
	require.NoError(t, err)
	require.False(t, res.IsError)
	var resp struct {
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))
	return resp.Findings
}

// TestDuplicateDefinitions_PythonMethods_NotFlagged — Python methods (bare token
// under class_definition/block/function_definition) must not be flagged; a
// genuine free-function duplicate still is.
func TestDuplicateDefinitions_PythonMethods_NotFlagged(t *testing.T) {
	tg := newCrossLangGraph(t, `
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file) VALUES
			('m.py/class_definition_0', NULL, 'Foo', 1, 0, 'm.py'),
			('m.py/class_definition_0/block/function_definition_0', NULL, 'new', 1, 0, 'm.py'),
			('m.py/class_definition_1', NULL, 'Bar', 1, 0, 'm.py'),
			('m.py/class_definition_1/block/function_definition_0', NULL, 'new', 1, 0, 'm.py'),
			('m.py/function_definition_0', NULL, 'dup_free', 1, 0, 'm.py'),
			('n.py/function_definition_0', NULL, 'dup_free', 1, 0, 'n.py');
		INSERT INTO node_defs (token, node_id) VALUES
			('Foo.new', 'm.py/class_definition_0/block/function_definition_0'),
			('new',     'm.py/class_definition_0/block/function_definition_0'),
			('Bar.new', 'm.py/class_definition_1/block/function_definition_0'),
			('new',     'm.py/class_definition_1/block/function_definition_0'),
			('dup_free', 'm.py/function_definition_0'),
			('dup_free', 'n.py/function_definition_0');
	`)
	var sawMethod, sawDupFree bool
	for _, f := range findingsFor(t, tg, "duplicate_definitions") {
		if f.NodeID == "m.py/class_definition_0/block/function_definition_0" ||
			f.NodeID == "m.py/class_definition_1/block/function_definition_0" {
			sawMethod = true
		}
		if f.NodeID == "m.py/function_definition_0" || f.NodeID == "n.py/function_definition_0" {
			sawDupFree = true
		}
	}
	assert.False(t, sawMethod, "Python method (bare `new` under class_definition) must not be a duplicate (mache-caaae9)")
	assert.True(t, sawDupFree, "a genuine free-function duplicate must still be flagged")
}

// TestDuplicateDefinitions_JSMethods_NotFlagged — JS methods (method_definition
// under class_body) must not be flagged.
func TestDuplicateDefinitions_JSMethods_NotFlagged(t *testing.T) {
	tg := newCrossLangGraph(t, `
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file) VALUES
			('m.js/class_declaration_0/class_body/method_definition_0', NULL, 'create', 1, 0, 'm.js'),
			('m.js/class_declaration_1/class_body/method_definition_0', NULL, 'create', 1, 0, 'm.js');
		INSERT INTO node_defs (token, node_id) VALUES
			('Foo.create', 'm.js/class_declaration_0/class_body/method_definition_0'),
			('create',     'm.js/class_declaration_0/class_body/method_definition_0'),
			('Bar.create', 'm.js/class_declaration_1/class_body/method_definition_0'),
			('create',     'm.js/class_declaration_1/class_body/method_definition_0');
	`)
	for _, f := range findingsFor(t, tg, "duplicate_definitions") {
		assert.NotContains(t, f.NodeID, "method_definition",
			"JS method (bare `create` under class_body) must not be a duplicate (mache-caaae9)")
	}
}

// TestDeadCode_LeylineTypesNotFlagged — dead_code must not flag TYPES (Rust
// struct_item/trait_item, Python class_definition) as dead now that leyline
// populates source_file; a genuinely-dead free function still is.
func TestDeadCode_LeylineTypesNotFlagged(t *testing.T) {
	tg := newCrossLangGraph(t, `
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file) VALUES
			('src/lib.rs/struct_item_0', NULL, 'Foo', 1, 0, 'src/lib.rs'),
			('src/lib.rs/trait_item', NULL, 'Runner', 1, 0, 'src/lib.rs'),
			('m.py/class_definition_0', NULL, 'Widget', 1, 0, 'm.py'),
			('src/lib.rs/function_item_1', NULL, 'never_called', 1, 0, 'src/lib.rs');
		INSERT INTO node_defs (token, node_id) VALUES
			('Foo', 'src/lib.rs/struct_item_0'),
			('Runner', 'src/lib.rs/trait_item'),
			('Widget', 'm.py/class_definition_0'),
			('never_called', 'src/lib.rs/function_item_1');
	`)
	var sawType, sawDeadFn bool
	for _, f := range findingsFor(t, tg, "dead_code") {
		switch f.NodeID {
		case "src/lib.rs/struct_item_0", "src/lib.rs/trait_item", "m.py/class_definition_0":
			sawType = true
		case "src/lib.rs/function_item_1":
			sawDeadFn = true
		}
	}
	assert.False(t, sawType, "dead_code must not flag types (struct_item/trait_item/class_definition) as dead (mache-caaae9)")
	assert.True(t, sawDeadFn, "a genuinely uncalled free function must still be flagged dead")
}
