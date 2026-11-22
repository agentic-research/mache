package mcpserve

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/testutil"
)

// deepFixture is a data projection four levels deep: groups/<g>/<module>/
// {name,doc}. With 2 groups of 3 modules that is 1 + 2 + 6 = 9 directories
// and 12 files, of which the old two-level count saw 3 directories and 0
// files (#820).
func deepFixture(t *testing.T) (schema *api.Topology, dataPath string) {
	t.Helper()
	schema = &api.Topology{
		Version: "v1",
		Nodes: []api.Node{{
			Name:     "groups",
			Selector: "$",
			Children: []api.Node{{
				Name:     "{{.group}}",
				Selector: "$[*]",
				Children: []api.Node{{
					Name:     "{{.name}}",
					Selector: "$.modules[*]",
					Files: []api.Leaf{
						{Name: "name", ContentTemplate: "{{.name}}"},
						{Name: "doc", ContentTemplate: "{{.doc}}"},
					},
				}},
			}},
		}},
	}
	dataPath = filepath.Join(t.TempDir(), "data.json")
	require.NoError(t, os.WriteFile(dataPath, []byte(`[
		{"group":"g1","modules":[{"name":"a","doc":"x"},{"name":"b","doc":"x"},{"name":"c","doc":"x"}]},
		{"group":"g2","modules":[{"name":"d","doc":"x"},{"name":"e","doc":"x"},{"name":"f","doc":"x"}]}
	]`), 0o644))
	return schema, dataPath
}

const deepDirs, deepFiles = 9, 12

type overviewTotals struct {
	TotalDirs       int  `json:"total_dirs"`
	TotalFiles      int  `json:"total_files"`
	TotalsTruncated bool `json:"totals_truncated"`
}

func overviewTotalsOf(t *testing.T, g graph.Graph) overviewTotals {
	t.Helper()
	res, err := makeGetOverviewHandler(g)(context.Background(), mcp.CallToolRequest{})
	require.NoError(t, err)
	var ov overviewTotals
	require.NoError(t, json.Unmarshal([]byte(testutil.ResultText(t, res)), &ov))
	return ov
}

// servedGraph is g as serve mode holds it: inside an initialized lazyGraph.
func servedGraph(g graph.Graph) *lazyGraph {
	lg := &lazyGraph{inner: g}
	lg.once.Do(func() {})
	lg.embedOnce.Do(func() {})
	return lg
}

// walkOnly hides every capability but Graph, as a backend that cannot count
// itself appears to get_overview.
type walkOnly struct{ graph.Graph }

// get_overview's totals are the whole graph's, whichever backend serves it:
// the in-memory store `mache serve data.json` builds, the nodes-table .db
// `mache --out` writes, and a graph that can only be walked.
func TestGetOverview_TotalsCountTheWholeGraph(t *testing.T) {
	schema, dataPath := deepFixture(t)

	store := graph.NewMemoryStore()
	require.NoError(t, ingest.NewEngine(schema, store).Ingest(dataPath))

	outDB := filepath.Join(t.TempDir(), "out.db")
	w, err := ingest.NewSQLiteWriter(outDB)
	require.NoError(t, err)
	require.NoError(t, ingest.NewEngine(schema, w).Ingest(dataPath))
	require.NoError(t, w.Close())
	sg, _, cleanup, err := openDBGraph(outDB, schema, graph.LockPerStatement, nil)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	for name, g := range map[string]graph.Graph{
		"memory store":       store,
		"nodes-table .db":    sg,
		"walk-only graph":    walkOnly{store},
		"serve-mode wrapper": servedGraph(store),
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, overviewTotals{TotalDirs: deepDirs, TotalFiles: deepFiles}, overviewTotalsOf(t, g))
		})
	}
}

// A walk that stops at its cap says so: a partial count is a lower bound,
// not a total.
func TestWalkCounts_ReportsTruncation(t *testing.T) {
	schema, dataPath := deepFixture(t)
	store := graph.NewMemoryStore()
	require.NoError(t, ingest.NewEngine(schema, store).Ingest(dataPath))
	roots, err := store.ListChildren("")
	require.NoError(t, err)

	dirs, files, truncated := walkCounts(store, roots, 5)
	assert.True(t, truncated)
	assert.Equal(t, 5, dirs+files)

	dirs, files, truncated = walkCounts(store, roots, deepDirs+deepFiles)
	assert.False(t, truncated, "a walk that visits exactly every node is complete")
	assert.Equal(t, deepDirs, dirs)
	assert.Equal(t, deepFiles, files)
}

// find_callers documents `token`; get_impact, find_definition and
// get_dataflow document `symbol`. Each accepts the other's spelling and
// answers as it does for its own (#820).
func TestSymbolAndTokenAreInterchangeable(t *testing.T) {
	g := testutil.BuildTestGraph(t)
	for tool, h := range map[string]func(graph.Graph) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error){
		"find_callers": func(g graph.Graph) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return makeFindCallersHandler(g)
		},
		"find_definition": func(g graph.Graph) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return makeFindDefinitionHandler(g)
		},
		"get_impact": func(g graph.Graph) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return makeGetImpactHandler(g)
		},
		"get_dataflow": func(g graph.Graph) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return makeGetDataflowHandler(g)
		},
	} {
		t.Run(tool, func(t *testing.T) {
			call := func(arg string) (string, bool) {
				res, err := h(g)(context.Background(), testutil.MakeRequest(map[string]any{arg: "Helper"}))
				require.NoError(t, err)
				return testutil.ResultText(t, res), res.IsError
			}
			bySymbol, symErr := call("symbol")
			byToken, tokErr := call("token")
			require.False(t, symErr, bySymbol)
			require.False(t, tokErr, byToken)
			assert.Equal(t, bySymbol, byToken)
			assert.Contains(t, bySymbol, "pkg/", "the fixture must have an answer to compare")

			res, err := h(g)(context.Background(), testutil.MakeRequest(nil))
			require.NoError(t, err)
			require.True(t, res.IsError)
			text := testutil.ResultText(t, res)
			assert.Contains(t, text, "symbol")
			assert.Contains(t, text, "token")
		})
	}
}
