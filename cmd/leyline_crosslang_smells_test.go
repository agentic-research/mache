package cmd

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// crossLangDef is one leyline-emitted definition: a token, the construct that
// defines it, and its κ kind.
type crossLangDef struct {
	token  string
	nodeID fixturedb.ConstructID
	kind   fixturedb.CanonicalKind
	name   string // nodes.name; defaults to the id's last segment
}

// newCrossLangGraph builds the fixture for these tests.
//
// It used to take a `ddl string` — a helper PARAMETERIZED ON SQL, on top of a
// hardcoded two-column node_defs. That was the mache-projection shape, while
// every node_id below is a real `leyline parse` path and the file's own header
// says so. The producer and the shape disagreed, and nothing in the test said
// which one it meant (mache-7555da).
func newCrossLangGraph(t *testing.T, defs []crossLangDef) *smellTestGraph {
	t.Helper()
	return newSmellFixture(t, fixturedb.Leyline, func(b *fixturedb.Builder) {
		for _, d := range defs {
			b.Construct(d.nodeID, fixturedb.Where{Name: d.name})
			b.Def(d.token, d.nodeID, d.kind)
		}
	})
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
	// leyline DUAL-REGISTERS a method: the qualified `Foo.new` AND the bare
	// `new`, both at the same construct. Two rows at one node_id is exactly
	// what ley-line's missing primary key permits and the mache projection's
	// PRIMARY KEY (token, node_id) would have silently collapsed.
	tg := newCrossLangGraph(t, []crossLangDef{
		{"Foo", "m.py/class_definition_0", fixturedb.Type, "Foo"},
		{"Foo.new", "m.py/class_definition_0/block/function_definition_0", fixturedb.Method, "new"},
		{"new", "m.py/class_definition_0/block/function_definition_0", fixturedb.Method, "new"},
		{"Bar", "m.py/class_definition_1", fixturedb.Type, "Bar"},
		{"Bar.new", "m.py/class_definition_1/block/function_definition_0", fixturedb.Method, "new"},
		{"new", "m.py/class_definition_1/block/function_definition_0", fixturedb.Method, "new"},
		{"dup_free", "m.py/function_definition_0", fixturedb.Function, "dup_free"},
		{"dup_free", "n.py/function_definition_0", fixturedb.Function, "dup_free"},
	})
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
	tg := newCrossLangGraph(t, []crossLangDef{
		{"Foo.create", "m.js/class_declaration_0/class_body/method_definition_0", fixturedb.Method, "create"},
		{"create", "m.js/class_declaration_0/class_body/method_definition_0", fixturedb.Method, "create"},
		{"Bar.create", "m.js/class_declaration_1/class_body/method_definition_0", fixturedb.Method, "create"},
		{"create", "m.js/class_declaration_1/class_body/method_definition_0", fixturedb.Method, "create"},
	})
	for _, f := range findingsFor(t, tg, "duplicate_definitions") {
		assert.NotContains(t, f.NodeID, "method_definition",
			"JS method (bare `create` under class_body) must not be a duplicate (mache-caaae9)")
	}
}

// TestDeadCode_LeylineTypesNotFlagged — dead_code must not flag TYPES (Rust
// struct_item/trait_item, Python class_definition) as dead now that leyline
// populates source_file; a genuinely-dead free function still is.
func TestDeadCode_LeylineTypesNotFlagged(t *testing.T) {
	tg := newCrossLangGraph(t, []crossLangDef{
		{"Foo", "src/lib.rs/struct_item_0", fixturedb.Type, "Foo"},
		{"Runner", "src/lib.rs/trait_item", fixturedb.Interface, "Runner"},
		{"Widget", "m.py/class_definition_0", fixturedb.Type, "Widget"},
		{"never_called", "src/lib.rs/function_item_1", fixturedb.Function, "never_called"},
	})
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
