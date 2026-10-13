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

// TestDefTokenFor_ResolvesLeylineIDsToTheirSymbol is the regression. Before
// the fix, both get_impact and get_dataflow derived the lookup token with
// filepath.Base(nodeID), which on a leyline projection yields
// "function_declaration" — a string that matches nothing real, and matches
// PROSE in markdown, which leyline also indexes. Measured over a real
// projection: 0 of 10,409 node_defs rows have an ID ending in their token.
// It holds on projection-v6 too, where the id the caller holds is a path and
// node_defs keys by nid.
func TestDefTokenFor_ResolvesLeylineIDsToTheirSymbol(t *testing.T) {
	for _, p := range []fixturedb.Producer{fixturedb.Leyline, fixturedb.LeylineV6} {
		got := graph.DefTokenFor(leylineShapedGraph(t, p), "a.go/function_declaration")
		require.Equal(t, "Alpha", got,
			"%v: the token must come from node_defs; the node ID's last segment is a construct kind, never the symbol", p)
		assert.NotContains(t, got, "function_declaration",
			"leaking the construct kind is what made get_impact match markdown code spans as call sites")
	}
}

// TestDefTokenFor_FallsBackForNodesThatDefineNothing covers directories, file
// nodes and virtual nodes: they have no node_defs row, so there is no better
// answer than the path segment.
func TestDefTokenFor_FallsBackForNodesThatDefineNothing(t *testing.T) {
	for _, p := range []fixturedb.Producer{fixturedb.Leyline, fixturedb.LeylineV6} {
		assert.Equal(t, "a.go", graph.DefTokenFor(leylineShapedGraph(t, p), "a.go"), "%v", p)
	}
}

// TestDefTokenFor_FallsBackWithoutASQLHandle covers a backend that is not a
// RefsQuerier at all (a plain MemoryStore). On the standalone mache schema the
// path segment IS the symbol, so the fallback is correct there — it is only
// wrong on leyline, which is what the fix distinguishes.
func TestDefTokenFor_FallsBackWithoutASQLHandle(t *testing.T) {
	assert.Equal(t, "Alpha", graph.DefTokenFor(graph.NewMemoryStore(), "pkg/functions/Alpha"))
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
