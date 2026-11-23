package mcpserve

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/agentic-research/mache/internal/leylinegraph"
	machetmpl "github.com/agentic-research/mache/internal/template"
	"github.com/agentic-research/mache/internal/testutil"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// leylineShapedGraph is a graph whose node IDs look like ley-line-open's:
// ending in a tree-sitter construct kind plus an ordinal, NOT in the symbol.
// That distinction is the entire bug (mache-cb4369), so the fixture has to
// carry it — a mache-schema fixture, where the ID ends in the symbol, cannot
// fail the way production did.
func leylineShapedGraph(t *testing.T, p fixturedb.Producer) graph.Graph {
	t.Helper()
	path, _ := fixturedb.New(t, p).
		Parsed("a.go/function_declaration").
		Def("Alpha", "a.go/function_declaration", fixturedb.Function).
		Build()
	sg, err := graph.OpenSQLiteGraph(path, &api.Topology{Version: api.SchemaVersion}, machetmpl.Render)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sg.Close() })
	return sg
}

// TestDefTokensFor_ResolvesLeylineIDsToTheirSymbol is the regression. Before
// the fix, both get_impact and get_dataflow derived the lookup token with
// filepath.Base(nodeID), which on a leyline projection yields
// "function_declaration" — a string that matches nothing real, and matches
// PROSE in markdown, which leyline also indexes. Measured over a real
// projection: 0 of 10,409 node_defs rows have an ID ending in their token.
// It holds on projection-v6 too, where the id the caller holds is a path and
// node_defs keys by nid.
func TestDefTokensFor_ResolvesLeylineIDsToTheirSymbol(t *testing.T) {
	for _, p := range []fixturedb.Producer{fixturedb.Leyline, fixturedb.LeylineV6} {
		got := graph.DefTokensFor(leylineShapedGraph(t, p), "a.go/function_declaration")
		require.Equal(t, []string{"Alpha"}, got,
			"%v: the token must come from node_defs; the node ID's last segment is a construct kind, never the symbol", p)
	}
}

// TestDefTokensFor_FallsBackForNodesThatDefineNothing covers directories, file
// nodes and virtual nodes: they have no node_defs row, so there is no better
// answer than the path segment.
func TestDefTokensFor_FallsBackForNodesThatDefineNothing(t *testing.T) {
	for _, p := range []fixturedb.Producer{fixturedb.Leyline, fixturedb.LeylineV6} {
		assert.Equal(t, []string{"a.go"}, graph.DefTokensFor(leylineShapedGraph(t, p), "a.go"), "%v", p)
	}
}

// TestDefTokensFor_FallsBackWithoutASQLHandle covers a backend that is not a
// RefsQuerier at all (a plain MemoryStore). On the standalone mache schema the
// path segment IS the symbol, so the fallback is correct there — it is only
// wrong on leyline, which is what the fix distinguishes.
func TestDefTokensFor_FallsBackWithoutASQLHandle(t *testing.T) {
	assert.Equal(t, []string{"Alpha"}, graph.DefTokensFor(graph.NewMemoryStore(), "pkg/functions/Alpha"))
}

// TestLeylineProjection_CallersImpactAndDataflowAgree exercises the real
// producer/consumer seam. A hand-built fixture can accidentally encode
// Mache-shaped IDs (ending in the symbol) and miss the production failure:
// Leyline IDs end in tree-sitter kinds such as function_declaration_0.
func TestLeylineProjection_CallersImpactAndDataflowAgree(t *testing.T) {
	testutil.RequirePinnedLeyline(t)

	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "sample.go"), []byte(`package sample

func Alpha() {}
func Beta() { Alpha() }
func Gamma() { Alpha() }
`), 0o644))

	dbPath, cleanup, err := leylinegraph.AutoInvokeLeylineParse(src)
	require.NoError(t, err)
	defer cleanup()

	sg, err := graph.OpenSQLiteGraph(dbPath, &api.Topology{Version: api.SchemaVersion}, machetmpl.Render)
	require.NoError(t, err)
	defer func() { require.NoError(t, sg.Close()) }()
	require.NoError(t, sg.EagerScan())

	callerResult, err := makeFindCallersHandler(sg)(context.Background(), testutil.MakeRequest(map[string]any{
		"token": "Alpha",
	}))
	require.NoError(t, err)
	require.False(t, callerResult.IsError, testutil.ResultText(t, callerResult))
	var callers []string
	require.NoError(t, json.Unmarshal([]byte(testutil.ResultText(t, callerResult)), &callers))
	sort.Strings(callers)
	require.NotEmpty(t, callers, "the real Leyline projection must contain Alpha call sites")

	impactResult, err := makeGetImpactHandler(sg)(context.Background(), testutil.MakeRequest(map[string]any{
		"symbol":    "Alpha",
		"kind":      "function",
		"direction": "callers",
		"depth":     1,
	}))
	require.NoError(t, err)
	require.False(t, impactResult.IsError, testutil.ResultText(t, impactResult))
	var impact struct {
		Nodes []struct {
			Path      string `json:"path"`
			Depth     int    `json:"depth"`
			Direction string `json:"direction"`
		} `json:"nodes"`
	}
	require.NoError(t, json.Unmarshal([]byte(testutil.ResultText(t, impactResult)), &impact))
	impactCallers := make([]string, 0)
	for _, node := range impact.Nodes {
		if node.Depth == 1 && node.Direction == "caller" {
			impactCallers = append(impactCallers, node.Path)
		}
	}
	sort.Strings(impactCallers)

	dataflowCallResult, err := makeGetDataflowHandler(sg)(context.Background(), testutil.MakeRequest(map[string]any{
		"symbol":    "Alpha",
		"kind":      "function",
		"direction": "callers",
		"depth":     1,
	}))
	require.NoError(t, err)
	require.False(t, dataflowCallResult.IsError, testutil.ResultText(t, dataflowCallResult))
	var flow dataflowResult
	require.NoError(t, json.Unmarshal([]byte(testutil.ResultText(t, dataflowCallResult)), &flow))
	flowCallers := make([]string, 0)
	for _, node := range flow.Nodes {
		if node.Depth == 1 {
			flowCallers = append(flowCallers, node.Path)
		}
	}
	sort.Strings(flowCallers)

	assert.Equal(t, callers, impactCallers,
		"get_impact must traverse the same real node_refs callers as find_callers")
	assert.Equal(t, callers, flowCallers,
		"get_dataflow must traverse the same real node_refs callers as find_callers")
	for _, path := range append(append([]string{}, impactCallers...), flowCallers...) {
		assert.False(t, strings.Contains(path, ".md/") || strings.Contains(path, "docs/"),
			"construct-kind lookup must not drift into Markdown prose: %s", path)
	}
}

// twoNameGraph is a module known by two tokens, its full name and its short
// one, where every reference uses the full name: the shape of a module in a
// data projection (mache-61faa0). The definitions are written in the given
// order, because the bug was that the answer followed storage order.
func twoNameGraph(t *testing.T, p fixturedb.Producer, defOrder ...string) graph.Graph {
	t.Helper()
	b := fixturedb.New(t, p).
		Parsed("a.go/function_declaration_0").
		Parsed("a.go/function_declaration_1").
		Parsed("a.go/function_declaration_2")
	b.ASTNode("a.go/function_declaration_1/block", "block", "a.go", fixturedb.Bytes(90, 160))
	b.ASTNode("a.go/function_declaration_1/block/call_expression", "call_expression", "a.go", fixturedb.Bytes(100, 110))
	b.ASTNode("a.go/function_declaration_2/block", "block", "a.go", fixturedb.Bytes(190, 260))
	b.ASTNode("a.go/function_declaration_2/block/call_expression", "call_expression", "a.go", fixturedb.Bytes(200, 210))
	for _, token := range defOrder {
		b.Def(token, "a.go/function_declaration_0", fixturedb.Function)
	}
	b.Def("CallerOne", "a.go/function_declaration_1", fixturedb.Function)
	b.Def("CallerTwo", "a.go/function_declaration_2", fixturedb.Function)
	b.Ref("Theory.EulerSum", "a.go/function_declaration_1", "a.go/function_declaration_1/block/call_expression", "")
	b.Ref("Theory.EulerSum", "a.go/function_declaration_2", "a.go/function_declaration_2/block/call_expression", "")
	path, _ := b.Build()
	sg, err := graph.OpenSQLiteGraph(path, &api.Topology{Version: api.SchemaVersion}, machetmpl.Render)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sg.Close() })
	return sg
}

// A node known by two names is called by whoever references either. One
// token used to be chosen, with LIMIT 1 and no order, so a module whose short
// name came first in node_defs had no callers according to get_impact while
// find_callers listed twenty (mache-61faa0). Whatever order the definitions
// were written in, every surface that derives callers from a node agrees
// with find_callers on the referenced name.
func TestCallersOfANodeWithTwoNames_DoNotDependOnDefinitionOrder(t *testing.T) {
	const node = "a.go/function_declaration_0"
	for _, p := range []fixturedb.Producer{fixturedb.Leyline, fixturedb.LeylineV6} {
		for name, order := range map[string][]string{
			"full name first":  {"Theory.EulerSum", "EulerSum"},
			"short name first": {"EulerSum", "Theory.EulerSum"},
		} {
			t.Run(name, func(t *testing.T) {
				g := twoNameGraph(t, p, order...)
				assert.Equal(t, []string{"EulerSum", "Theory.EulerSum"}, graph.DefTokensFor(g, node), "every token, sorted")

				want, err := g.GetCallers("Theory.EulerSum")
				require.NoError(t, err)
				require.Len(t, want, 2, "%v: the fixture must have callers under the full name", p)
				wantIDs := nodeIDs(want)

				got, err := graph.CallersOf(g, node, nil)
				require.NoError(t, err)
				assert.Equal(t, wantIDs, nodeIDs(got), "%v: CallersOf", p)

				impact := callTool(t, makeGetImpactHandler(g), map[string]any{"symbol": "Theory.EulerSum", "direction": "callers", "depth": 1})
				for _, id := range wantIDs {
					assert.Contains(t, impact, id, "%v: get_impact", p)
				}
				flow := callTool(t, makeGetDataflowHandler(g), map[string]any{"symbol": "Theory.EulerSum", "direction": "callers", "depth": 1})
				for _, id := range wantIDs {
					assert.Contains(t, flow, id, "%v: get_dataflow", p)
				}
			})
		}
	}
}

// follow leaves a token out of the lookup, and a caller reached through two
// tokens is listed once.
func TestCallersOf_FollowAndDedup(t *testing.T) {
	store := graph.NewMemoryStore()
	g := twoTokens{store}
	store.AddNode(&graph.Node{ID: "caller"})
	require.NoError(t, store.AddRef("Long.Name", "caller"))
	require.NoError(t, store.AddRef("Name", "caller"))

	both, err := graph.CallersOf(g, "x", nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"caller"}, nodeIDs(both), "one caller, reached by both tokens, listed once")

	none, err := graph.CallersOf(g, "x", func(string) bool { return false })
	require.NoError(t, err)
	assert.Empty(t, none)
}

// twoTokens is a graph whose every node defines the same two tokens.
type twoTokens struct{ *graph.MemoryStore }

func (twoTokens) DefTokensOf(string) ([]string, error) { return []string{"Long.Name", "Name"}, nil }

// nodeIDs is the ids of nodes, sorted.
func nodeIDs(nodes []*graph.Node) []string {
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	sort.Strings(ids)
	return ids
}

// callTool runs a handler and returns its text, failing on a tool error.
func callTool(t *testing.T, h server.ToolHandlerFunc, args map[string]any) string {
	t.Helper()
	res, err := h(context.Background(), testutil.MakeRequest(args))
	require.NoError(t, err)
	text := testutil.ResultText(t, res)
	require.False(t, res.IsError, text)
	return text
}
