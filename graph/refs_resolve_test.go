package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/fixturedb"
)

// resolverFor builds a leyline-shaped projection and opens it the way a
// consumer does — graph.Open, not an internal constructor — so the test
// exercises the same path modmap uses.
func resolverFor(t *testing.T, build func(*fixturedb.Builder)) *SQLiteGraph {
	t.Helper()
	b := fixturedb.New(t, fixturedb.Leyline)
	build(b)
	path, _ := b.Build()

	g, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })
	return g
}

// TestResolveRef_PicksTheBareTokenRowOfADualEmit is the regression for the bug
// that only real data exposed: a node_id is NOT a unique key into node_refs.
// A qualified call emits TWO rows at the same node — `pkg.Fn` and `Fn` — and
// 37,236 node_ids in this repo's own projection carry more than one.
//
// node_defs is keyed on the BARE token, so selecting the qualified row reports
// a false "no target" for a reference that resolves perfectly well. An
// arbitrary LIMIT 1 picked wrong roughly half the time.
func TestResolveRef_PicksTheBareTokenRowOfADualEmit(t *testing.T) {
	const site = fixturedb.SiteID("caller.go/function_declaration_0/call_expression")

	// Token names chosen so ALPHABETICAL order picks the WRONG row: "a.run"
	// sorts before "run". An earlier version of this test used Helper /
	// helper.Helper, where uppercase sorts first and the bare token won by
	// accident — the test passed even with the fix reverted, which makes it
	// worse than no test.
	g := resolverFor(t, func(b *fixturedb.Builder) {
		b.Def("run", "a/impl.go/function_declaration_0", "function")
		b.Import("a", "example.com/mod/a", "caller.go")
		// The dual-emit pair, both at ONE node id. Per LLO's table contract the
		// qualifier rides the bare-token row; the qualified row carries none.
		b.Ref("a.run", "caller.go/function_declaration_0", site, "")
		b.Ref("run", "caller.go/function_declaration_0", site, "a")
	})

	got, err := g.ResolveRef(string(site))
	require.NoError(t, err)
	assert.Equal(t, RefResolved, got.Resolution,
		"the bare-token row resolves; picking the qualified row would report a false no-target")
	assert.Equal(t, "a/impl.go/function_declaration_0", got.NodeID)
}

// TestResolveRef_ClassifiesEveryOutcome walks the four Resolution values. They
// exist because "no node id" means four different things to a consumer walking
// a blast radius, and a boolean would collapse 89% of references into one
// undifferentiated bucket.
func TestResolveRef_ClassifiesEveryOutcome(t *testing.T) {
	t.Run("external: qualified by an import, no local definition", func(t *testing.T) {
		const site = fixturedb.SiteID("caller.go/function_declaration_0/call_expression")
		g := resolverFor(t, func(b *fixturedb.Builder) {
			b.Import("fmt", "fmt", "caller.go")
			b.Ref("Errorf", "caller.go/function_declaration_0", site, "fmt")
		})

		got, err := g.ResolveRef(string(site))
		require.NoError(t, err)
		assert.Equal(t, RefExternal, got.Resolution,
			"a call leaving the projection has ZERO local blast radius — that is an answer, not a failure")
		assert.Equal(t, "fmt", got.Via)
		assert.Empty(t, got.NodeID, "an unresolved target must expose nothing consumable")
	})

	t.Run("no-target: a receiver variable is not an import", func(t *testing.T) {
		const site = fixturedb.SiteID("caller.go/function_declaration_0/call_expression")
		g := resolverFor(t, func(b *fixturedb.Builder) {
			b.Import("fmt", "fmt", "caller.go")
			// `t.Errorf` — the qualifier is a local variable, matching no import.
			b.Ref("Errorf", "caller.go/function_declaration_0", site, "t")
		})

		got, err := g.ResolveRef(string(site))
		require.NoError(t, err)
		assert.Equal(t, RefNoTarget, got.Resolution)
		assert.Empty(t, got.Via, "a variable receiver must not be reported as an import")
	})

	t.Run("ambiguous: several definitions, candidates returned", func(t *testing.T) {
		const site = fixturedb.SiteID("caller.go/function_declaration_0/call_expression")
		g := resolverFor(t, func(b *fixturedb.Builder) {
			b.Def("Run", "alpha.go/function_declaration_0", "function")
			b.Def("Run", "beta.go/function_declaration_0", "function")
			b.Ref("Run", "caller.go/function_declaration_0", site, "")
		})

		got, err := g.ResolveRef(string(site))
		require.NoError(t, err)
		assert.Equal(t, RefAmbiguous, got.Resolution)
		assert.Len(t, got.Candidates, 2)
		assert.Empty(t, got.NodeID,
			"ambiguity must never be resolved by silently picking the first candidate")
	})
}

// TestResolveRef_NarrowingIsAFilterNotATiebreak pins the rule that keeps this
// honest: package narrowing may REDUCE candidates, but if it does not reduce to
// exactly one the caller still gets ambiguity. A filter that promotes itself to
// a tiebreak is how a wrong answer acquires confidence.
func TestResolveRef_NarrowingIsAFilterNotATiebreak(t *testing.T) {
	const site = fixturedb.SiteID("caller.go/function_declaration_0/call_expression")
	g := resolverFor(t, func(b *fixturedb.Builder) {
		// Two same-named defs BOTH inside the imported package.
		b.Def("Run", "helper/alpha.go/function_declaration_0", "function")
		b.Def("Run", "helper/beta.go/function_declaration_0", "function")
		b.Import("helper", "example.com/mod/helper", "caller.go")
		b.Ref("Run", "caller.go/function_declaration_0", site, "helper")
	})

	got, err := g.ResolveRef(string(site))
	require.NoError(t, err)
	assert.Equal(t, RefAmbiguous, got.Resolution,
		"narrowing to a package that still holds two candidates must stay ambiguous")
	assert.Len(t, got.Candidates, 2)
}

// TestHasPathSegment_MatchesWholeSegmentsOnly guards the narrowing filter
// against substring matching, which would let "graph" claim "subgraph/x.go".
func TestHasPathSegment_MatchesWholeSegmentsOnly(t *testing.T) {
	assert.True(t, hasPathSegment("graph/graph.go", "graph"))
	assert.True(t, hasPathSegment("a/graph/b.go", "graph"))
	assert.True(t, hasPathSegment("graph", "graph"))
	assert.False(t, hasPathSegment("subgraph/x.go", "graph"),
		"a substring match would attribute definitions to the wrong package")
	assert.False(t, hasPathSegment("graphing/x.go", "graph"))
}

// TestRefRangeOf_ConvertsToOneBased pins the boundary that costs everyone a bug
// once: `_ast` rows are tree-sitter 0-based, editors are 1-based.
func TestRefRangeOf_ConvertsToOneBased(t *testing.T) {
	const site = fixturedb.SiteID("caller.go/function_declaration_0/call_expression")
	g := resolverFor(t, func(b *fixturedb.Builder) {
		b.Ref("Run", "caller.go/function_declaration_0", site, "")
		b.ASTNode(string(site), "call_expression", "caller.go",
			fixturedb.Span{StartByte: 100, EndByte: 140, StartRow: 6, StartCol: 2, EndRow: 6, EndCol: 42})
	})

	got, err := g.RefRangeOf(string(site))
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, uint32(7), got.StartLine, "tree-sitter row 6 is the 7th line")
	assert.Equal(t, uint32(3), got.StartCol, "column 2 is the 3rd column")
	assert.Equal(t, uint32(7), got.EndLine)
	assert.Equal(t, uint32(100), got.StartByte, "byte offsets carry through UNCHANGED")
	assert.Equal(t, uint32(140), got.EndByte)
	assert.Equal(t, "caller.go", got.SourceID)
}

// TestRefRangeOf_AbsentNodeIsNilNotZero: a zero-valued range reads as "line 0
// of an empty file" to every consumer, which is worse than admitting we do not
// know.
func TestRefRangeOf_AbsentNodeIsNilNotZero(t *testing.T) {
	g := resolverFor(t, func(b *fixturedb.Builder) {
		b.Def("Run", "alpha.go/function_declaration_0", "function")
	})
	got, err := g.RefRangeOf("nope/does_not_exist")
	require.NoError(t, err)
	assert.Nil(t, got)
}
