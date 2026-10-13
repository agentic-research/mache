package mcpserve

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/agentic-research/mache/internal/testutil"
)

// calledFunctionDB is a leyline-shaped .db where Beta calls Alpha.
func calledFunctionDB(t *testing.T, p fixturedb.Producer) string {
	b := fixturedb.New(t, p)
	b.Source("a.go", "go", "")
	b.ASTNode("a.go", "source_file", "a.go", fixturedb.Bytes(0, 200))
	b.ASTNode("a.go/function_declaration_0", "function_declaration", "a.go", fixturedb.Bytes(10, 60))
	b.ASTNode("a.go/function_declaration_1", "function_declaration", "a.go", fixturedb.Bytes(70, 160))
	b.ASTNode("a.go/function_declaration_1/block", "block", "a.go", fixturedb.Bytes(90, 160))
	b.ASTNode("a.go/function_declaration_1/block/call_expression", "call_expression", "a.go", fixturedb.Bytes(100, 110))
	b.Def("Alpha", "a.go/function_declaration_0", fixturedb.Function)
	b.Def("Beta", "a.go/function_declaration_1", fixturedb.Function)
	b.Ref("Alpha", "a.go/function_declaration_1", "a.go/function_declaration_1/block/call_expression", "")
	path, _ := b.Build()
	return path
}

// A leyline .db is served whichever projection wrote it. On projection-v6
// virtual nodes are not materialized into the producer's arena
// (mache-abaf17), and the graph serves without them.
func TestOpenDBGraph_ServesBothProjections(t *testing.T) {
	for _, p := range []fixturedb.Producer{fixturedb.Leyline, fixturedb.LeylineV6} {
		g, _, cleanup, err := openDBGraph(calledFunctionDB(t, p), &api.Topology{Version: api.SchemaVersion}, nil)
		require.NoError(t, err, "%v", p)
		kids, err := g.ListChildren("a.go")
		require.NoError(t, err)
		assert.Contains(t, kids, "a.go/function_declaration_0", "%v", p)
		cleanup()
	}
}

// list_directory offers callers/ on a construct that is called. On a leyline
// projection the construct's last path segment is a kind, so the token it
// looks callers up by must come from node_defs (mache-cb4369).
func TestListDirectory_OffersCallersOnALeylineConstruct(t *testing.T) {
	for _, p := range []fixturedb.Producer{fixturedb.Leyline, fixturedb.LeylineV6} {
		g, _, cleanup, err := openDBGraph(calledFunctionDB(t, p), &api.Topology{Version: api.SchemaVersion}, nil)
		require.NoError(t, err)
		res, err := makeListDirHandler(g)(context.Background(),
			testutil.MakeRequest(map[string]any{"path": "a.go/function_declaration_0"}))
		require.NoError(t, err)
		require.False(t, res.IsError, testutil.ResultText(t, res))
		var entries []nodeEntry
		require.NoError(t, json.Unmarshal([]byte(testutil.ResultText(t, res)), &entries))
		var names []string
		for _, e := range entries {
			names = append(names, e.Name)
		}
		assert.Contains(t, names, "callers", "%v: Alpha is called by Beta", p)
		cleanup()
	}
}
