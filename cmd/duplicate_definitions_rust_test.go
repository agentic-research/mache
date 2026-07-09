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

// mache-22fecf — duplicate_definitions false-positives on leyline-parsed Rust
// impl/trait methods. The rule's method exclusion (`node_id NOT LIKE
// '%/methods/%'`) is specific to the mache Go-schema, which files methods under
// a `methods/` path segment and renders their tokens as `Receiver.Name`.
// leyline's AST-native projection instead:
//   - renders method tokens BARE (`new`, `compute`, `run` — no receiver), and
//   - files them under `impl_item_N/declaration_list/function_item_M` (Rust)
//     / `trait_item/declaration_list/function_signature_item` (trait sigs),
//     with NO `methods/` segment.
// So the same method name across N impls collides by token and the `methods/`
// exclusion never fires — every constructor (`new`) and interface method
// (`run`) implemented by N types is reported as a bogus "duplicate definition".
//
// Ground truth captured from `leyline parse` of a two-struct/one-trait Rust
// file; this fixture reproduces the exact node_id shapes it emits.

// rustDupFixture builds a nodes + node_defs db mirroring leyline's Rust output.
func rustDupFixture(t *testing.T) *smellTestGraph {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "rustdup.db"))
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

		-- Rust impl/trait methods: bare tokens, impl_item / trait_item paths.
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file) VALUES
			('src/lib.rs/impl_item_0/declaration_list/function_item_0', NULL, 'new',     1, 0, 'src/lib.rs'),
			('src/lib.rs/impl_item_1/declaration_list/function_item_0', NULL, 'new',     1, 0, 'src/lib.rs'),
			('src/lib.rs/impl_item_0/declaration_list/function_item_1', NULL, 'compute', 1, 0, 'src/lib.rs'),
			('src/lib.rs/impl_item_1/declaration_list/function_item_1', NULL, 'compute', 1, 0, 'src/lib.rs'),
			('src/lib.rs/impl_item_2/declaration_list/function_item',   NULL, 'run',     1, 0, 'src/lib.rs'),
			('src/lib.rs/impl_item_3/declaration_list/function_item',   NULL, 'run',     1, 0, 'src/lib.rs'),
			('src/lib.rs/trait_item/declaration_list/function_signature_item', NULL, 'run', 1, 0, 'src/lib.rs'),
			-- A genuine free-function duplicate (top-level function_item in two
			-- files): this SHOULD still be flagged — the fix must be targeted,
			-- not a blanket disable of duplicate detection.
			('a.rs/function_item_0', NULL, 'dup_free', 1, 0, 'a.rs'),
			('b.rs/function_item_0', NULL, 'dup_free', 1, 0, 'b.rs'),
			-- A unique free function: never flagged (copies == 1).
			('src/lib.rs/function_item_9', NULL, 'solo', 1, 0, 'src/lib.rs');

		INSERT INTO node_defs (token, node_id) VALUES
			('new',      'src/lib.rs/impl_item_0/declaration_list/function_item_0'),
			('new',      'src/lib.rs/impl_item_1/declaration_list/function_item_0'),
			('compute',  'src/lib.rs/impl_item_0/declaration_list/function_item_1'),
			('compute',  'src/lib.rs/impl_item_1/declaration_list/function_item_1'),
			('run',      'src/lib.rs/impl_item_2/declaration_list/function_item'),
			('run',      'src/lib.rs/impl_item_3/declaration_list/function_item'),
			('run',      'src/lib.rs/trait_item/declaration_list/function_signature_item'),
			('dup_free', 'a.rs/function_item_0'),
			('dup_free', 'b.rs/function_item_0'),
			('solo',     'src/lib.rs/function_item_9');
	`)
	require.NoError(t, err)
	return &smellTestGraph{db: db}
}

func runDuplicateDefinitions(t *testing.T, tg *smellTestGraph) []smellFinding {
	t.Helper()
	handler := makeFindSmellsHandler(tg)
	res, err := handler(context.Background(), makeRequest(map[string]any{
		"rule": "duplicate_definitions", "limit": float64(1000),
	}))
	require.NoError(t, err)
	require.False(t, res.IsError)
	var resp struct {
		Findings []smellFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))
	return resp.Findings
}

// TestDuplicateDefinitions_RustImplMethods_NotFlagged pins mache-22fecf: Rust
// impl/trait methods (bare tokens under impl_item/trait_item, same name across
// types) must NOT be reported as duplicate definitions.
func TestDuplicateDefinitions_RustImplMethods_NotFlagged(t *testing.T) {
	findings := runDuplicateDefinitions(t, rustDupFixture(t))

	tokens := make(map[string]bool)
	for _, f := range findings {
		tokens[tokenFromNodeID(f.NodeID)] = true
	}
	for _, method := range []string{"new", "compute", "run"} {
		assert.NotContainsf(t, tokens, method,
			"Rust impl/trait method %q must not be flagged as a duplicate definition (mache-22fecf)", method)
	}
}

// TestDuplicateDefinitions_RustFreeFunctionDup_StillFlagged is the regression
// guard: the method-exclusion fix must not disable genuine free-function
// duplicate detection.
func TestDuplicateDefinitions_RustFreeFunctionDup_StillFlagged(t *testing.T) {
	findings := runDuplicateDefinitions(t, rustDupFixture(t))

	var sawDupFree bool
	for _, f := range findings {
		if tokenFromNodeID(f.NodeID) == "dup_free" {
			sawDupFree = true
		}
	}
	assert.True(t, sawDupFree,
		"a genuine free-function duplicate (top-level function_item, same token in two files) must still be flagged")
}

// tokenFromNodeID extracts the last path segment's leaf identity from a leyline
// node_id for assertion convenience. For '.../function_item_0' it returns the
// def's token via the nodes.name — but findings only carry node_id, so we map
// the known fixture node_ids back to their token.
func tokenFromNodeID(nodeID string) string {
	switch nodeID {
	case "src/lib.rs/impl_item_0/declaration_list/function_item_0",
		"src/lib.rs/impl_item_1/declaration_list/function_item_0":
		return "new"
	case "src/lib.rs/impl_item_0/declaration_list/function_item_1",
		"src/lib.rs/impl_item_1/declaration_list/function_item_1":
		return "compute"
	case "src/lib.rs/impl_item_2/declaration_list/function_item",
		"src/lib.rs/impl_item_3/declaration_list/function_item",
		"src/lib.rs/trait_item/declaration_list/function_signature_item":
		return "run"
	case "a.rs/function_item_0", "b.rs/function_item_0":
		return "dup_free"
	case "src/lib.rs/function_item_9":
		return "solo"
	}
	return nodeID
}

// TestDuplicateDefinitions_LeylineGoMethods_NotFlagged pins that the fix is not
// Rust-only: leyline parses GO methods the same way — bare token under
// `method_declaration_N`, NO `methods/` segment — so LLO-Go methods collide by
// token exactly like Rust. (The mache Go-SCHEMA's `methods/` path is a
// tree-sitter+go-schema artifact, absent from every leyline projection.)
func TestDuplicateDefinitions_LeylineGoMethods_NotFlagged(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "llogo.db"))
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

		-- LLO-Go methods: bare token under method_declaration_N (Foo.New, Bar.New, ...).
		INSERT INTO nodes (id, parent_id, name, kind, mtime, source_file) VALUES
			('main.go/method_declaration_0', NULL, 'New',     1, 0, 'main.go'),
			('main.go/method_declaration_2', NULL, 'New',     1, 0, 'main.go'),
			('main.go/method_declaration_1', NULL, 'Compute', 1, 0, 'main.go'),
			('main.go/method_declaration_3', NULL, 'Compute', 1, 0, 'main.go'),
			-- Genuine free-function duplicate (top-level function_declaration in
			-- two files) — must still be flagged.
			('a.go/function_declaration_0', NULL, 'DupFree', 1, 0, 'a.go'),
			('b.go/function_declaration_0', NULL, 'DupFree', 1, 0, 'b.go');

		INSERT INTO node_defs (token, node_id) VALUES
			('New',     'main.go/method_declaration_0'),
			('New',     'main.go/method_declaration_2'),
			('Compute', 'main.go/method_declaration_1'),
			('Compute', 'main.go/method_declaration_3'),
			('DupFree', 'a.go/function_declaration_0'),
			('DupFree', 'b.go/function_declaration_0');
	`)
	require.NoError(t, err)

	findings := runDuplicateDefinitions(t, &smellTestGraph{db: db})

	var sawDupFree bool
	for _, f := range findings {
		assert.NotContainsf(t, f.NodeID, "method_declaration",
			"LLO-Go method %q must not be flagged as a duplicate definition (mache-22fecf)", f.NodeID)
		if f.NodeID == "a.go/function_declaration_0" || f.NodeID == "b.go/function_declaration_0" {
			sawDupFree = true
		}
	}
	assert.True(t, sawDupFree, "a genuine free-function duplicate must still be flagged")
}
