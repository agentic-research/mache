package cmd

import (
	"testing"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// leylineShapedGraph is a graph whose node IDs look like ley-line-open's:
// ending in a tree-sitter construct kind plus an ordinal, NOT in the symbol.
// That distinction is the entire bug (mache-cb4369), so the fixture has to
// carry it — a mache-schema fixture, where the ID ends in the symbol, cannot
// fail the way production did.
func leylineShapedGraph(t *testing.T) *smellTestGraph {
	t.Helper()
	_, f := fixturedb.New(t, fixturedb.Leyline).
		Construct("a.go/function_declaration_0").
		Def("Alpha", "a.go/function_declaration_0", fixturedb.Function).
		Build()
	return &smellTestGraph{MemoryStore: graph.NewMemoryStore(), db: f.DB()}
}

// TestTokenForNode_ResolvesLeylineIDsToTheirSymbol is the regression. Before
// the fix, both get_impact and get_dataflow derived the lookup token with
// filepath.Base(nodeID), which on a leyline projection yields
// "function_declaration_0" — a string that matches nothing real, and matches
// PROSE in markdown, which leyline also indexes. Measured over a real
// projection: 0 of 10,409 node_defs rows have an ID ending in their token.
func TestTokenForNode_ResolvesLeylineIDsToTheirSymbol(t *testing.T) {
	got := tokenForNode(leylineShapedGraph(t), "a.go/function_declaration_0")

	require.Equal(t, "Alpha", got,
		"the token must come from node_defs; the node ID's last segment is a construct kind, never the symbol")
	assert.NotContains(t, got, "function_declaration",
		"leaking the construct kind is what made get_impact match markdown code spans as call sites")
}

// TestTokenForNode_FallsBackForNodesThatDefineNothing covers directories, file
// nodes and virtual nodes: they have no node_defs row, so there is no better
// answer than the path segment.
func TestTokenForNode_FallsBackForNodesThatDefineNothing(t *testing.T) {
	assert.Equal(t, "a.go", tokenForNode(leylineShapedGraph(t), "a.go"))
}

// TestTokenForNode_FallsBackWithoutASQLHandle covers a backend that is not a
// RefsQuerier at all (a plain MemoryStore). On the standalone mache schema the
// path segment IS the symbol, so the fallback is correct there — it is only
// wrong on leyline, which is what the fix distinguishes.
func TestTokenForNode_FallsBackWithoutASQLHandle(t *testing.T) {
	assert.Equal(t, "Alpha", tokenForNode(graph.NewMemoryStore(), "pkg/functions/Alpha"))
}
