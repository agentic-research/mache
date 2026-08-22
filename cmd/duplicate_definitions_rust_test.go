package cmd

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// rustDupFixture mirrors leyline's Rust output. fixturedb.Leyline is what makes
// "mirrors leyline" a fact rather than a claim — the DDL it produces is derived
// from the pinned binary's own sqlite_master (mache-7555da).
func rustDupFixture(t *testing.T) *smellTestGraph {
	t.Helper()
	return newSmellFixture(t, fixturedb.Leyline, func(b *fixturedb.Builder) {
		for _, d := range []struct {
			token string
			in    fixturedb.ConstructID
			kind  fixturedb.CanonicalKind
		}{
			// Rust impl/trait methods: bare tokens, impl_item / trait_item paths.
			{"new", "src/lib.rs/impl_item_0/declaration_list/function_item_0", fixturedb.Method},
			{"new", "src/lib.rs/impl_item_1/declaration_list/function_item_0", fixturedb.Method},
			{"compute", "src/lib.rs/impl_item_0/declaration_list/function_item_1", fixturedb.Method},
			{"compute", "src/lib.rs/impl_item_1/declaration_list/function_item_1", fixturedb.Method},
			{"run", "src/lib.rs/impl_item_2/declaration_list/function_item", fixturedb.Method},
			{"run", "src/lib.rs/impl_item_3/declaration_list/function_item", fixturedb.Method},
			{"run", "src/lib.rs/trait_item/declaration_list/function_signature_item", fixturedb.Method},
			// A genuine free-function duplicate (top-level function_item in two
			// files): this SHOULD still be flagged — the fix must be targeted,
			// not a blanket disable of duplicate detection.
			{"dup_free", "a.rs/function_item_0", fixturedb.Function},
			{"dup_free", "b.rs/function_item_0", fixturedb.Function},
			// A unique free function: never flagged (copies == 1).
			{"solo", "src/lib.rs/function_item_9", fixturedb.Function},
		} {
			b.Construct(d.in) // symbol lives on Def; nodes.name is the id's last segment
			b.Def(d.token, d.in, d.kind)
		}
	})
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
	tg := newSmellFixture(t, fixturedb.Leyline, func(b *fixturedb.Builder) {
		for _, d := range []struct {
			token string
			in    fixturedb.ConstructID
			kind  fixturedb.CanonicalKind
		}{
			// LLO-Go methods: bare token under method_declaration_N.
			{"New", "main.go/method_declaration_0", fixturedb.Method},
			{"New", "main.go/method_declaration_2", fixturedb.Method},
			{"Compute", "main.go/method_declaration_1", fixturedb.Method},
			{"Compute", "main.go/method_declaration_3", fixturedb.Method},
			// Genuine free-function duplicate (top-level function_declaration in
			// two files) — must still be flagged.
			{"DupFree", "a.go/function_declaration_0", fixturedb.Function},
			{"DupFree", "b.go/function_declaration_0", fixturedb.Function},
		} {
			b.Construct(d.in) // symbol lives on Def; nodes.name is the id's last segment
			b.Def(d.token, d.in, d.kind)
		}
	})

	findings := runDuplicateDefinitions(t, tg)

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
