package cmd

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	// fixturedb.Leyline is what makes the comment above TRUE rather than
	// aspirational. The hand-written DDL this replaced described itself as "the
	// Leyline column shape" while omitting node_refs.qualifier, omitting
	// node_defs.container_node_id, and ADDING a PRIMARY KEY ... WITHOUT ROWID
	// that ley-line does not have — so ensureCanonicalViews ran a different
	// v_refs body here than in production (mache-e3f3bf, mache-7555da).
	return newSmellFixture(t, fixturedb.Leyline, func(b *fixturedb.Builder) {
		// Uncovered exported Go function — the one expected finding.
		b.Def("Orphan", "pkg/orphan.go/function_declaration_0", fixturedb.Function)
		// Covered ONLY by a test caller: no TestServed def exists, so
		// coverage must come from the container_node_id join arm.
		b.Def("Served", "pkg/served.go/function_declaration_0", fixturedb.Function)
		// The test function that calls Served.
		b.Def("TestServedEndToEnd", "pkg/served_test.go/function_declaration_0", fixturedb.Function)
		// The non-test caller that refs Orphan (must NOT grant coverage).
		b.Def("runAll", "pkg/run.go/function_declaration_0", fixturedb.Function)
		// Rust function: canonical_kind='function' too, but the TestFoo
		// convention is Go-only — the .go scope must exclude it.
		b.Def("RustHelper", "src/lib.rs/function_item_0", fixturedb.Function)
		// testdata corpus: Go by extension, excluded by directory.
		b.Def("FixtureFn", "testdata/sample.go/function_declaration_0", fixturedb.Function)
		b.Def("NestedFixtureFn", "internal/x/testdata/deep.go/function_declaration_0", fixturedb.Function)
		// Method: uppercase, uncovered, but canonical_kind='method'.
		b.Def("Handle", "pkg/recv.go/method_declaration_0", fixturedb.Method)

		// Served is called from INSIDE TestServedEndToEnd. `from` is the
		// enclosing test function; `at` is the call-site leaf. On ley-line
		// those are different columns, and the caller identity is only
		// recoverable from the first — which is exactly why they are two
		// parameters now instead of one implicit convention.
		b.Ref("Served", "pkg/served_test.go/function_declaration_0",
			"pkg/served_test.go/function_declaration_0/block/statement_list/expression_statement/call_expression", "")
		// Orphan is called — but from a NON-test function. Must not count.
		b.Ref("Orphan", "pkg/run.go/function_declaration_0",
			"pkg/run.go/function_declaration_0/block/statement_list/expression_statement/call_expression", "")
	})
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
