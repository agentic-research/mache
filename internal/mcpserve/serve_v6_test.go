package mcpserve

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/agentic-research/mache/internal/testutil"
	"github.com/mark3labs/mcp-go/server"
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

// withSchema is a served graph as the registry hands it to a handler: one that
// knows the schema it was built with. It embeds the concrete graph so every
// capability the registry's lazyGraph forwards (DefTokenOf among them) still
// shows through.
type withSchema struct {
	*graph.SQLiteGraph
	schema *api.Topology
}

func (w withSchema) Schema() *api.Topology { return w.schema }

// virtualAnswers is what an MCP client sees of the virtual paths a mount shows,
// over a .db where Beta calls Alpha.
func virtualAnswers(t *testing.T, p fixturedb.Producer) map[string]string {
	t.Helper()
	dbPath := calledFunctionDB(t, p)
	schema := &api.Topology{Version: api.SchemaVersion}
	sg, _, cleanup, err := openDBGraph(dbPath, schema, nil)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	g := withSchema{SQLiteGraph: sg.(*graph.SQLiteGraph), schema: schema}
	call := func(h server.ToolHandlerFunc, args map[string]any) string {
		res, err := h(context.Background(), testutil.MakeRequest(args))
		require.NoError(t, err)
		require.False(t, res.IsError, "%v: %v: %s", p, args, testutil.ResultText(t, res))
		return testutil.ResultText(t, res)
	}
	const alpha = "a.go/function_declaration_0"
	out := map[string]string{
		"root":        call(makeListDirHandler(g), map[string]any{"path": ""}),
		"schema":      call(makeReadFileHandler(g), map[string]any{"path": "_schema.json"}),
		"callers":     call(makeListDirHandler(g), map[string]any{"path": alpha + "/callers"}),
		"caller":      call(makeReadFileHandler(g), map[string]any{"path": alpha + "/callers/a.go_function_declaration_1_block_call_expression"}),
		"caller/miss": testutil.ResultText(t, must(makeReadFileHandler(g)(context.Background(), testutil.MakeRequest(map[string]any{"path": alpha + "/callers/nope"})))),
	}

	// Serving answers these at runtime; it writes none of them into the .db.
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	// v4 names a node in nodes.name; v6 interns it.
	q := `SELECT COUNT(*) FROM nodes WHERE name IN ('callers', '_schema.json')`
	if p == fixturedb.LeylineV6 {
		q = `SELECT COUNT(*) FROM nodes n JOIN names m ON m.name_id = n.name_id
			WHERE m.text IN ('callers', '_schema.json')`
	}
	var written int
	require.NoError(t, db.QueryRow(q).Scan(&written))
	assert.Zero(t, written, "%v: serving wrote virtual rows into the .db", p)
	return out
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// The virtual paths a mount shows answer the same over MCP from either
// projection, through the mount's own resolver, and serving writes none of
// them into the .db (mache-abaf17). On projection-v6, where mache cannot write
// them, callers/ used to list as empty and _schema.json was not found.
func TestVirtualPaths_SameAnswersFromProjectionV4AndV6(t *testing.T) {
	v4 := virtualAnswers(t, fixturedb.Leyline)
	v6 := virtualAnswers(t, fixturedb.LeylineV6)

	var root []nodeEntry
	require.NoError(t, json.Unmarshal([]byte(v4["root"]), &root))
	var rootNames []string
	for _, e := range root {
		rootNames = append(rootNames, e.Name)
	}
	assert.Equal(t, []string{"_schema.json", "a.go"}, rootNames, "_schema.json where byte order puts it, as when it was a row")
	assert.Contains(t, v4["schema"], `"version"`)
	var callers []nodeEntry
	require.NoError(t, json.Unmarshal([]byte(v4["callers"]), &callers))
	require.Len(t, callers, 1, "Beta calls Alpha")
	assert.Equal(t, "a.go_function_declaration_1_block_call_expression", callers[0].Name, "named for the call site, as find_callers answers on a leyline projection")
	assert.Equal(t, "file", callers[0].Type)
	assert.Contains(t, v4["caller/miss"], "not found")

	for k := range v4 {
		assert.Equal(t, v4[k], v6[k], k)
	}
}
