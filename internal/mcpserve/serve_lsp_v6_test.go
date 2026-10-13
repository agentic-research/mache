package mcpserve

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/agentic-research/mache/internal/testutil"
	"github.com/mark3labs/mcp-go/server"
)

// The LSP tools read ley-line-open's _lsp, _lsp_hover and _lsp_defs, which
// projection-v6 keys by nid where v4 keyed them by node_id (mache-b1f7b8). One
// enriched spec is built as both shapes, and every LSP tool must answer the
// same from each.

const (
	lspAdd      = "store.go/method_declaration"
	lspNew      = "store.go/function_declaration"
	lspAddHover = "symbols/Store/Add"
	lspNewHover = "symbols/NewStore"
)

func enrichedDB(t *testing.T, p fixturedb.Producer) string {
	b := fixturedb.New(t, p)
	b.Source("store.go", "go", "")
	b.ASTNode("store.go", "source_file", "store.go", fixturedb.Bytes(0, 200))
	b.ASTNode(lspNew, "function_declaration", "store.go", fixturedb.Bytes(10, 60))
	b.ASTNode(lspAdd, "method_declaration", "store.go", fixturedb.Bytes(70, 160))
	b.Def("NewStore", lspNew, fixturedb.Function)
	b.Def("Add", lspAdd, fixturedb.Method)
	b.LSPHover(lspAddHover, "func (s *Store) Add(k string)")
	b.LSPHover(lspNewHover, "func NewStore() *Store")
	b.LSPSymbol(lspAdd, "Method", `[{"message":"k declared and not used"}]`)
	b.LSPSymbol(lspNew, "Function", "")
	b.LSPDef("NewStore", lspNew, "file:///store.go", 1, 5, 1, 13)
	path, _ := b.Build()
	return path
}

func openEnriched(t *testing.T, p fixturedb.Producer) graph.Graph {
	t.Helper()
	g, _, cleanup, err := openDBGraph(enrichedDB(t, p), &api.Topology{Version: api.SchemaVersion}, nil)
	require.NoError(t, err, "%v", p)
	t.Cleanup(cleanup)
	return g
}

// lspAnswers is every LSP tool's answer over g, as text.
func lspAnswers(t *testing.T, g graph.Graph) map[string]string {
	t.Helper()
	call := func(h server.ToolHandlerFunc, args map[string]any) string {
		res, err := h(context.Background(), testutil.MakeRequest(args))
		require.NoError(t, err)
		require.False(t, res.IsError, testutil.ResultText(t, res))
		return testutil.ResultText(t, res)
	}
	defs, err := queryLSPDefs(g.(graph.RefsQuerier), "NewStore")
	require.NoError(t, err)
	defsJSON, err := json.Marshal(defs)
	require.NoError(t, err)
	return map[string]string{
		"type_info":            call(makeGetTypeInfoHandler(g), map[string]any{"symbol": "Add"}),
		"type_info/lowercase":  call(makeGetTypeInfoHandler(g), map[string]any{"symbol": "newstore"}),
		"type_info/miss":       call(makeGetTypeInfoHandler(g), map[string]any{"symbol": "Nope"}),
		"diagnostics":          call(makeGetDiagnosticsHandler(g), map[string]any{}),
		"diagnostics/symbol":   call(makeGetDiagnosticsHandler(g), map[string]any{"symbol": "store.go"}),
		"diagnostics/no-match": call(makeGetDiagnosticsHandler(g), map[string]any{"symbol": "other.go"}),
		"lsp_defs":             string(defsJSON),
	}
}

func TestLSPTools_SameAnswersFromProjectionV4AndV6(t *testing.T) {
	v4 := lspAnswers(t, openEnriched(t, fixturedb.Leyline))
	v6 := lspAnswers(t, openEnriched(t, fixturedb.LeylineV6))

	// The v4 side must say something real, or the comparison is vacuous.
	var hover []hoverResult
	require.NoError(t, json.Unmarshal([]byte(v4["type_info"]), &hover))
	assert.Equal(t, []hoverResult{{NodeID: lspAddHover, HoverText: "func (s *Store) Add(k string)"}}, hover,
		"hover is found by the symbol's standalone address, and only that symbol's")
	assert.Contains(t, v4["type_info/lowercase"], lspNewHover, "matching folds case, as LIKE did")
	assert.Contains(t, v4["type_info/miss"], "no type info found")
	var diags []diagResult
	require.NoError(t, json.Unmarshal([]byte(v4["diagnostics"]), &diags))
	assert.Equal(t, []diagResult{{NodeID: lspAdd, SymbolKind: "Method", Diagnostics: `[{"message":"k declared and not used"}]`}}, diags,
		"only the symbol that carries diagnostics")
	assert.Equal(t, v4["diagnostics"], v4["diagnostics/symbol"])
	assert.Contains(t, v4["diagnostics/no-match"], "no diagnostics found")
	assert.Contains(t, v4["lsp_defs"], `"node_id":"`+lspNew+`"`)

	for k := range v4 {
		assert.Equal(t, v4[k], v6[k], k)
	}
}

// The daemon's query op sends every value as text: an integer in decimal, a
// blob as hex, NULL as null (sql_value_to_json in LLO's daemon). The same
// lookups must read a v6 arena through that wire.
func TestLSPLookups_ReadAV6ArenaThroughTheDaemonWire(t *testing.T) {
	g := openEnriched(t, fixturedb.LeylineV6)
	direct := graphLSPSource(g.(graph.RefsQuerier))
	overJSON := func(query string) ([][]any, error) {
		rows, err := direct(query)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			for i, v := range r {
				switch x := v.(type) {
				case int64:
					r[i] = strconv.FormatInt(x, 10)
				case float64:
					r[i] = strconv.FormatFloat(x, 'f', -1, 64)
				case []byte:
					r[i] = hex.EncodeToString(x)
				}
			}
		}
		return rows, nil
	}

	hover, err := lookupHover(overJSON, "Add", "")
	require.NoError(t, err)
	assert.Equal(t, []hoverResult{{NodeID: lspAddHover, HoverText: "func (s *Store) Add(k string)"}}, hover)

	diags, err := lookupDiagnostics(overJSON, "", "", 50)
	require.NoError(t, err)
	require.Len(t, diags, 1)
	assert.Equal(t, lspAdd, diags[0].NodeID)

	defs, err := lookupLSPDefs(overJSON, "NewStore")
	require.NoError(t, err)
	require.Len(t, defs, 1)
	assert.Equal(t, lspDefLocation{NodeID: lspNew, URI: "file:///store.go", StartLine: 1, StartCol: 5, EndLine: 1, EndCol: 13}, defs[0])

	none, err := lookupDiagnostics(overJSON, "", "", 0)
	require.NoError(t, err)
	assert.Empty(t, none, "limit 0 is no rows, as LIMIT 0 was")
}
