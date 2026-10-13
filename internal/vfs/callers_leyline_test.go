package vfs

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/fixturedb"
)

// On a leyline projection a construct's path ends in its kind
// (function_declaration_0), never its symbol, so callers/ must look callers up
// by the token node_defs records for the construct (mache-cb4369). It used the
// last path segment, so the mount's callers/ never appeared on one.
func TestCallersHandler_FindsCallersOnALeylineProjection(t *testing.T) {
	for _, p := range []fixturedb.Producer{fixturedb.Leyline, fixturedb.LeylineV6} {
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
		g, err := graph.OpenSQLiteGraph(path, &api.Topology{Version: api.SchemaVersion}, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = g.Close() })

		h := &CallersHandler{Graph: g}
		assert.Equal(t, []DirExtra{{Name: graph.CallersDir, Kind: KindDir, Perm: 0o555}},
			h.DirExtras("/a.go/function_declaration_0", nil), "%v", p)
		entries, ok := h.ListDir("/a.go/function_declaration_0/callers")
		require.True(t, ok, "%v", p)
		require.Len(t, entries, 1, "%v", p)
		assert.Equal(t, "a.go_function_declaration_1_block_call_expression", entries[0].Name, "%v", p)
		e := h.Stat("/a.go/function_declaration_0/callers/" + entries[0].Name)
		require.NotNil(t, e, "%v", p)
		assert.Equal(t, "a.go/function_declaration_1/block/call_expression", e.NodeID, "%v", p)
		assert.Nil(t, h.DirExtras("/a.go/function_declaration_1", nil), "%v: nothing calls Beta", p)
	}
}
