// Package testutil is the shared test substrate for the cmd decomposition
// (mache-96c378 stage 1). Before it existed, every helper here lived in one
// cmd _test.go file and was used by dozens of others — which pinned all 108
// test files into package cmd. Helpers take testing.TB and are exported;
// nothing here may import cmd or any package extracted from it, so the
// dependency arrow only ever points downward.
package testutil

import (
	"io/fs"
	"testing"

	"github.com/agentic-research/mache/graph"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// BuildTestGraph creates a MemoryStore with a predictable tree:
//
//	pkg/
//	  main/
//	    source       -> "func main() {}"
//	    context      -> "package main"
//	  util/
//	    helper/
//	      source     -> "func Helper() {}"
//	empty/
//
// Refs: "Helper" -> ["pkg/util/helper"]
// Defs: "Helper" -> ["pkg/util/helper"]
func BuildTestGraph(t testing.TB) *graph.MemoryStore {
	t.Helper()
	store := graph.NewMemoryStore()

	// Root
	store.AddRoot(&graph.Node{
		ID:       "pkg",
		Mode:     fs.ModeDir,
		Children: []string{"pkg/main", "pkg/util"},
	})

	// pkg/main dir
	store.AddNode(&graph.Node{
		ID:       "pkg/main",
		Mode:     fs.ModeDir,
		Children: []string{"pkg/main/source"},
		Context:  []byte("package main"),
	})
	store.AddNode(&graph.Node{
		ID:   "pkg/main/source",
		Mode: 0,
		Data: []byte("func main() {}"),
	})

	// pkg/util dir
	store.AddNode(&graph.Node{
		ID:       "pkg/util",
		Mode:     fs.ModeDir,
		Children: []string{"pkg/util/helper"},
	})
	store.AddNode(&graph.Node{
		ID:       "pkg/util/helper",
		Mode:     fs.ModeDir,
		Children: []string{"pkg/util/helper/source"},
	})
	store.AddNode(&graph.Node{
		ID:   "pkg/util/helper/source",
		Mode: 0,
		Data: []byte("func Helper() {}"),
	})

	// empty dir (no children)
	store.AddRoot(&graph.Node{
		ID:       "empty",
		Mode:     fs.ModeDir,
		Children: []string{},
	})

	// Refs: "Helper" is referenced by pkg/main/source
	require.NoError(t, store.AddRef("Helper", "pkg/main/source"))
	// Defs: "Helper" is defined in pkg/util/helper
	require.NoError(t, store.AddDef("Helper", "pkg/util/helper"))

	return store
}

// ResultText extracts the text from the first content item of a CallToolResult.
func ResultText(t testing.TB, result *mcp.CallToolResult) string {
	t.Helper()
	require.NotNil(t, result)
	require.NotEmpty(t, result.Content, "result should have content")
	tc, ok := result.Content[0].(mcp.TextContent)
	require.True(t, ok, "first content should be TextContent, got %T", result.Content[0])
	return tc.Text
}

// MakeRequest constructs a CallToolRequest with the given arguments.
func MakeRequest(args map[string]any) mcp.CallToolRequest {
	var req mcp.CallToolRequest
	req.Params.Arguments = args
	return req
}
