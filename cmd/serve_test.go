package cmd

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/graph"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/mcptest"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Test helper: build a known graph fixture
// ---------------------------------------------------------------------------

// buildTestGraph creates a MemoryStore with a predictable tree:
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
func buildTestGraph(t *testing.T) *graph.MemoryStore {
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

// resultText extracts the text from the first content item of a CallToolResult.
func resultText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	require.NotNil(t, result)
	require.NotEmpty(t, result.Content, "result should have content")
	tc, ok := result.Content[0].(mcp.TextContent)
	require.True(t, ok, "first content should be TextContent, got %T", result.Content[0])
	return tc.Text
}

// makeRequest constructs a CallToolRequest with the given arguments.
func makeRequest(args map[string]any) mcp.CallToolRequest {
	var req mcp.CallToolRequest
	req.Params.Arguments = args
	return req
}

// ---------------------------------------------------------------------------
// list_directory handler tests
// ---------------------------------------------------------------------------

func TestListDir_Root(t *testing.T) {
	store := buildTestGraph(t)
	handler := makeListDirHandler(store)

	result, err := handler(context.Background(), makeRequest(map[string]any{"path": ""}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	text := resultText(t, result)
	var entries []nodeEntry
	require.NoError(t, json.Unmarshal([]byte(text), &entries))

	// Root should have "pkg" and "empty"
	assert.Len(t, entries, 2)
	names := map[string]string{}
	for _, e := range entries {
		names[e.Name] = e.Type
	}
	assert.Equal(t, "dir", names["pkg"])
	assert.Equal(t, "dir", names["empty"])
}

func TestListDir_Subdir(t *testing.T) {
	store := buildTestGraph(t)
	handler := makeListDirHandler(store)

	result, err := handler(context.Background(), makeRequest(map[string]any{"path": "pkg"}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	var entries []nodeEntry
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &entries))

	assert.Len(t, entries, 2)
	names := map[string]string{}
	for _, e := range entries {
		names[e.Name] = e.Type
	}
	assert.Equal(t, "dir", names["main"])
	assert.Equal(t, "dir", names["util"])
}

func TestListDir_IncludesFiles(t *testing.T) {
	store := buildTestGraph(t)
	handler := makeListDirHandler(store)

	result, err := handler(context.Background(), makeRequest(map[string]any{"path": "pkg/main"}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	var entries []nodeEntry
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &entries))

	assert.Len(t, entries, 1)
	assert.Equal(t, "source", entries[0].Name)
	assert.Equal(t, "file", entries[0].Type)
	assert.Equal(t, int64(14), entries[0].Size) // len("func main() {}")
}

func TestListDir_Empty(t *testing.T) {
	store := buildTestGraph(t)
	handler := makeListDirHandler(store)

	result, err := handler(context.Background(), makeRequest(map[string]any{"path": "empty"}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	var entries []nodeEntry
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &entries))
	assert.Empty(t, entries)
}

func TestListDir_NotFound(t *testing.T) {
	store := buildTestGraph(t)
	handler := makeListDirHandler(store)

	result, err := handler(context.Background(), makeRequest(map[string]any{"path": "nonexistent"}))
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, resultText(t, result), "nonexistent")
}

func TestListDir_DefaultEmptyPath(t *testing.T) {
	store := buildTestGraph(t)
	handler := makeListDirHandler(store)

	// No "path" arg at all — should default to root
	result, err := handler(context.Background(), makeRequest(nil))
	require.NoError(t, err)
	require.False(t, result.IsError)

	var entries []nodeEntry
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &entries))
	assert.Len(t, entries, 2)
}

func TestListDir_ExcludeTests(t *testing.T) {
	store := graph.NewMemoryStore()

	// Create a package with test and non-test constructs
	store.AddRoot(&graph.Node{
		ID:       "go/graph/functions",
		Mode:     fs.ModeDir,
		Children: []string{"go/graph/functions/NewMemoryStore", "go/graph/functions/TestMemoryStore_AddRoot", "go/graph/functions/BenchmarkScan", "go/graph/functions/compileLevels"},
	})
	for _, name := range []string{"NewMemoryStore", "TestMemoryStore_AddRoot", "BenchmarkScan", "compileLevels"} {
		store.AddNode(&graph.Node{
			ID:   "go/graph/functions/" + name,
			Mode: fs.ModeDir,
		})
	}

	handler := makeListDirHandler(store)

	// Without filter: all 4
	result, err := handler(context.Background(), makeRequest(map[string]any{"path": "go/graph/functions"}))
	require.NoError(t, err)
	var all []nodeEntry
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &all))
	assert.Len(t, all, 4)

	// With exclude_tests: only NewMemoryStore and compileLevels
	result, err = handler(context.Background(), makeRequest(map[string]any{"path": "go/graph/functions", "exclude_tests": true}))
	require.NoError(t, err)
	var filtered []nodeEntry
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &filtered))
	assert.Len(t, filtered, 2)
	names := make([]string, len(filtered))
	for i, e := range filtered {
		names[i] = e.Name
	}
	assert.Contains(t, names, "NewMemoryStore")
	assert.Contains(t, names, "compileLevels")
}

// ---------------------------------------------------------------------------
// read_file handler tests
// ---------------------------------------------------------------------------

func TestReadFile_Success(t *testing.T) {
	store := buildTestGraph(t)
	handler := makeReadFileHandler(store)

	result, err := handler(context.Background(), makeRequest(map[string]any{"path": "pkg/main/source"}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	assert.Equal(t, "func main() {}", resultText(t, result))
}

func TestReadFile_NotFound(t *testing.T) {
	store := buildTestGraph(t)
	handler := makeReadFileHandler(store)

	result, err := handler(context.Background(), makeRequest(map[string]any{"path": "nonexistent"}))
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, resultText(t, result), "not found")
}

func TestReadFile_IsDirectory(t *testing.T) {
	store := buildTestGraph(t)
	handler := makeReadFileHandler(store)

	result, err := handler(context.Background(), makeRequest(map[string]any{"path": "pkg/main"}))
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, resultText(t, result), "directory")
}

func TestReadFile_RequiredPath(t *testing.T) {
	store := buildTestGraph(t)
	handler := makeReadFileHandler(store)

	// Empty path
	result, err := handler(context.Background(), makeRequest(map[string]any{"path": ""}))
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, resultText(t, result), "required")
}

func TestReadFile_EmptyContent(t *testing.T) {
	store := graph.NewMemoryStore()
	store.AddRoot(&graph.Node{
		ID:       "dir",
		Mode:     fs.ModeDir,
		Children: []string{"dir/empty-file"},
	})
	store.AddNode(&graph.Node{
		ID:   "dir/empty-file",
		Mode: 0,
		Data: nil,
	})

	handler := makeReadFileHandler(store)
	result, err := handler(context.Background(), makeRequest(map[string]any{"path": "dir/empty-file"}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	assert.Equal(t, "", resultText(t, result))
}

// ---------------------------------------------------------------------------
// find_callers handler tests
// ---------------------------------------------------------------------------

func TestFindCallers_Found(t *testing.T) {
	store := buildTestGraph(t)
	handler := makeFindCallersHandler(store)

	result, err := handler(context.Background(), makeRequest(map[string]any{"token": "Helper"}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	var paths []string
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &paths))
	assert.Contains(t, paths, "pkg/main/source")
}

func TestFindCallers_NotFound(t *testing.T) {
	store := buildTestGraph(t)
	handler := makeFindCallersHandler(store)

	result, err := handler(context.Background(), makeRequest(map[string]any{"token": "NonExistent"}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	assert.Equal(t, "[]", resultText(t, result))
}

func TestFindCallers_RequiredToken(t *testing.T) {
	store := buildTestGraph(t)
	handler := makeFindCallersHandler(store)

	result, err := handler(context.Background(), makeRequest(map[string]any{"token": ""}))
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, resultText(t, result), "required")
}

// ---------------------------------------------------------------------------
// find_callees handler tests
// ---------------------------------------------------------------------------

func TestFindCallees_EmptyWithoutExtractor(t *testing.T) {
	store := buildTestGraph(t)
	handler := makeFindCalleesHandler(store)

	// Without a CallExtractor set, GetCallees returns nil — handler returns JSON with hint
	result, err := handler(context.Background(), makeRequest(map[string]any{"path": "pkg/main"}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	assert.Contains(t, resultText(t, result), `"callees"`)
	assert.Contains(t, resultText(t, result), `"hint"`)
}

func TestFindCallees_RequiredPath(t *testing.T) {
	store := buildTestGraph(t)
	handler := makeFindCalleesHandler(store)

	result, err := handler(context.Background(), makeRequest(map[string]any{"path": ""}))
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, resultText(t, result), "required")
}

// ---------------------------------------------------------------------------
// search handler tests
// ---------------------------------------------------------------------------

func TestSearch_Found(t *testing.T) {
	store := buildTestGraph(t)
	require.NoError(t, store.InitRefsDB())
	defer func() { _ = store.Close() }()
	require.NoError(t, store.FlushRefs())

	handler := makeSearchHandler(store)

	result, err := handler(context.Background(), makeRequest(map[string]any{"pattern": "Helper"}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	type searchResult struct {
		Token string `json:"token"`
		Path  string `json:"path"`
	}
	var results []searchResult
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &results))
	assert.NotEmpty(t, results)
	assert.Equal(t, "Helper", results[0].Token)
}

func TestSearch_NoResults(t *testing.T) {
	store := buildTestGraph(t)
	require.NoError(t, store.InitRefsDB())
	defer func() { _ = store.Close() }()
	require.NoError(t, store.FlushRefs())

	handler := makeSearchHandler(store)

	result, err := handler(context.Background(), makeRequest(map[string]any{"pattern": "ZZZ_NO_MATCH_%"}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	var results []json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &results))
	assert.Empty(t, results)
}

func TestSearch_RequiredPattern(t *testing.T) {
	store := buildTestGraph(t)
	require.NoError(t, store.InitRefsDB())
	defer func() { _ = store.Close() }()
	require.NoError(t, store.FlushRefs())

	handler := makeSearchHandler(store)

	result, err := handler(context.Background(), makeRequest(map[string]any{"pattern": ""}))
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, resultText(t, result), "required")
}

func TestSearch_WithLimit(t *testing.T) {
	store := graph.NewMemoryStore()
	// Add many refs
	for i := 0; i < 20; i++ {
		require.NoError(t, store.AddRef("Token", "path/"+string(rune('A'+i))))
	}
	require.NoError(t, store.InitRefsDB())
	defer func() { _ = store.Close() }()
	require.NoError(t, store.FlushRefs())

	handler := makeSearchHandler(store)

	result, err := handler(context.Background(), makeRequest(map[string]any{
		"pattern": "Token",
		"limit":   float64(5), // JSON numbers are float64
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	type searchResult struct {
		Token string `json:"token"`
		Path  string `json:"path"`
	}
	var results []searchResult
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &results))
	assert.Len(t, results, 5)
}

func TestSearch_WildcardPattern(t *testing.T) {
	store := graph.NewMemoryStore()
	require.NoError(t, store.AddRef("FuncA", "a.go"))
	require.NoError(t, store.AddRef("FuncB", "b.go"))
	require.NoError(t, store.AddRef("TypeC", "c.go"))
	require.NoError(t, store.InitRefsDB())
	defer func() { _ = store.Close() }()
	require.NoError(t, store.FlushRefs())

	handler := makeSearchHandler(store)

	result, err := handler(context.Background(), makeRequest(map[string]any{"pattern": "Func%"}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	type searchResult struct {
		Token string `json:"token"`
		Path  string `json:"path"`
	}
	var results []searchResult
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &results))
	assert.Len(t, results, 2)
}

func TestSearch_DefinitionDedup(t *testing.T) {
	store := graph.NewMemoryStore()
	// Simulate how tree-sitter ingestion adds both bare and pkg-qualified defs
	// for the same construct — they should be deduped by path.
	require.NoError(t, store.AddDef("GetCallers", "go/graph/methods/MemoryStore.GetCallers"))
	require.NoError(t, store.AddDef("graph.MemoryStore.GetCallers", "go/graph/methods/MemoryStore.GetCallers"))
	require.NoError(t, store.AddDef("MemoryStore.GetCallers", "go/graph/methods/MemoryStore.GetCallers"))

	handler := makeSearchHandler(store)

	result, err := handler(context.Background(), makeRequest(map[string]any{
		"pattern": "%GetCallers%",
		"role":    "definition",
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	type searchResult struct {
		Token string `json:"token"`
		Path  string `json:"path"`
	}
	var results []searchResult
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &results))
	// All three tokens point to the same path — should be deduped to 1 result
	assert.Len(t, results, 1)
	assert.Equal(t, "go/graph/methods/MemoryStore.GetCallers", results[0].Path)
}

// ---------------------------------------------------------------------------
// get_communities handler tests
// ---------------------------------------------------------------------------

func TestGetCommunities_Found(t *testing.T) {
	store := graph.NewMemoryStore()
	// Create two clusters: a1,a2,a3 share "alpha","beta"; b1,b2,b3 share "gamma","delta"
	require.NoError(t, store.AddRef("alpha", "a1"))
	require.NoError(t, store.AddRef("alpha", "a2"))
	require.NoError(t, store.AddRef("alpha", "a3"))
	require.NoError(t, store.AddRef("beta", "a1"))
	require.NoError(t, store.AddRef("beta", "a2"))
	require.NoError(t, store.AddRef("beta", "a3"))
	require.NoError(t, store.AddRef("gamma", "b1"))
	require.NoError(t, store.AddRef("gamma", "b2"))
	require.NoError(t, store.AddRef("gamma", "b3"))
	require.NoError(t, store.AddRef("delta", "b1"))
	require.NoError(t, store.AddRef("delta", "b2"))
	require.NoError(t, store.AddRef("delta", "b3"))

	handler := makeGetCommunitiesHandler(store)
	result, err := handler(context.Background(), makeRequest(nil))
	require.NoError(t, err)
	require.False(t, result.IsError)

	var cr graph.CommunityResult
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &cr))
	assert.Equal(t, 6, cr.NumNodes)
	assert.Len(t, cr.Communities, 2)
	assert.Greater(t, cr.Modularity, 0.0)
}

func TestGetCommunities_Empty(t *testing.T) {
	store := graph.NewMemoryStore()
	handler := makeGetCommunitiesHandler(store)

	result, err := handler(context.Background(), makeRequest(nil))
	require.NoError(t, err)
	require.False(t, result.IsError)
	// Empty refs should return a diagnostic object, not bare "[]"
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &out))
	assert.Contains(t, out, "message")
	assert.Empty(t, out["communities"])
}

func TestGetCommunities_CustomMinSize(t *testing.T) {
	store := graph.NewMemoryStore()
	require.NoError(t, store.AddRef("t1", "a"))
	require.NoError(t, store.AddRef("t1", "b"))

	handler := makeGetCommunitiesHandler(store)

	// Min size 2 → includes the pair
	result, err := handler(context.Background(), makeRequest(map[string]any{"min_size": float64(2)}))
	require.NoError(t, err)
	var cr graph.CommunityResult
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &cr))
	assert.Len(t, cr.Communities, 1)

	// Min size 10 → filters it out
	result, err = handler(context.Background(), makeRequest(map[string]any{"min_size": float64(10)}))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &cr))
	assert.Empty(t, cr.Communities)
}

// ---------------------------------------------------------------------------
// registerMCPTools tests (multi-tenant: all tools registered unconditionally)
// ---------------------------------------------------------------------------

func TestRegisterMCPTools_AllToolsRegistered(t *testing.T) {
	store := buildTestGraph(t)
	require.NoError(t, store.InitRefsDB())
	defer func() { _ = store.Close() }()
	require.NoError(t, store.FlushRefs())

	registry := newGraphRegistry(".", nil)
	registry.graphs.Store(".", &lazyGraph{inner: store})

	s := server.NewMCPServer("test", "1.0.0", server.WithToolCapabilities(false))
	registerMCPTools(s, registry)

	// Use HandleMessage to list tools (avoids mcptest transport)
	reqJSON := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	resp := s.HandleMessage(context.Background(), reqJSON)
	respJSON, err := json.Marshal(resp)
	require.NoError(t, err)

	var result struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(respJSON, &result))

	toolNames := map[string]bool{}
	for _, tool := range result.Result.Tools {
		toolNames[tool.Name] = true
	}

	// In multi-tenant mode, all tools are registered unconditionally
	assert.True(t, toolNames["list_directory"])
	assert.True(t, toolNames["read_file"])
	assert.True(t, toolNames["find_callers"])
	assert.True(t, toolNames["find_callees"])
	assert.True(t, toolNames["search"], "all tools registered in multi-tenant mode")
	assert.True(t, toolNames["get_communities"], "all tools registered in multi-tenant mode")
	assert.True(t, toolNames["get_overview"])
	assert.True(t, toolNames["find_definition"])
	assert.True(t, toolNames["get_type_info"])
	assert.True(t, toolNames["get_diagnostics"])
	assert.True(t, toolNames["write_file"])
}

// ---------------------------------------------------------------------------
// Full MCP roundtrip via mcptest
// ---------------------------------------------------------------------------

func TestMCPRoundtrip_ListDirectory(t *testing.T) {
	store := buildTestGraph(t)
	require.NoError(t, store.InitRefsDB())
	defer func() { _ = store.Close() }()
	require.NoError(t, store.FlushRefs())

	srv := mcptest.NewUnstartedServer(t)
	addMacheTools(srv, store)
	require.NoError(t, srv.Start(context.Background()))
	defer srv.Close()

	client := srv.Client()
	req := mcp.CallToolRequest{}
	req.Params.Name = "list_directory"
	req.Params.Arguments = map[string]any{"path": ""}

	result, err := client.CallTool(context.Background(), req)
	require.NoError(t, err)
	require.False(t, result.IsError)

	tc, ok := result.Content[0].(mcp.TextContent)
	require.True(t, ok)

	var entries []nodeEntry
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &entries))
	assert.Len(t, entries, 2) // pkg, empty
}

func TestMCPRoundtrip_ReadFile(t *testing.T) {
	store := buildTestGraph(t)
	require.NoError(t, store.InitRefsDB())
	defer func() { _ = store.Close() }()
	require.NoError(t, store.FlushRefs())

	srv := mcptest.NewUnstartedServer(t)
	addMacheTools(srv, store)
	require.NoError(t, srv.Start(context.Background()))
	defer srv.Close()

	client := srv.Client()
	req := mcp.CallToolRequest{}
	req.Params.Name = "read_file"
	req.Params.Arguments = map[string]any{"path": "pkg/main/source"}

	result, err := client.CallTool(context.Background(), req)
	require.NoError(t, err)
	require.False(t, result.IsError)

	tc, ok := result.Content[0].(mcp.TextContent)
	require.True(t, ok)
	assert.Equal(t, "func main() {}", tc.Text)
}

func TestMCPRoundtrip_FindCallers(t *testing.T) {
	store := buildTestGraph(t)
	require.NoError(t, store.InitRefsDB())
	defer func() { _ = store.Close() }()
	require.NoError(t, store.FlushRefs())

	srv := mcptest.NewUnstartedServer(t)
	addMacheTools(srv, store)
	require.NoError(t, srv.Start(context.Background()))
	defer srv.Close()

	client := srv.Client()
	req := mcp.CallToolRequest{}
	req.Params.Name = "find_callers"
	req.Params.Arguments = map[string]any{"token": "Helper"}

	result, err := client.CallTool(context.Background(), req)
	require.NoError(t, err)
	require.False(t, result.IsError)

	tc, ok := result.Content[0].(mcp.TextContent)
	require.True(t, ok)

	var paths []string
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &paths))
	assert.Contains(t, paths, "pkg/main/source")
}

func TestMCPRoundtrip_Search(t *testing.T) {
	store := buildTestGraph(t)
	require.NoError(t, store.InitRefsDB())
	defer func() { _ = store.Close() }()
	require.NoError(t, store.FlushRefs())

	srv := mcptest.NewUnstartedServer(t)
	addMacheTools(srv, store)
	require.NoError(t, srv.Start(context.Background()))
	defer srv.Close()

	client := srv.Client()
	req := mcp.CallToolRequest{}
	req.Params.Name = "search"
	req.Params.Arguments = map[string]any{"pattern": "%elp%"}

	result, err := client.CallTool(context.Background(), req)
	require.NoError(t, err)
	require.False(t, result.IsError)

	tc, ok := result.Content[0].(mcp.TextContent)
	require.True(t, ok)

	type searchResult struct {
		Token string `json:"token"`
		Path  string `json:"path"`
	}
	var results []searchResult
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &results))
	assert.NotEmpty(t, results)
	assert.Equal(t, "Helper", results[0].Token)
}

func TestMCPRoundtrip_ErrorPropagation(t *testing.T) {
	store := buildTestGraph(t)
	require.NoError(t, store.InitRefsDB())
	defer func() { _ = store.Close() }()
	require.NoError(t, store.FlushRefs())

	srv := mcptest.NewUnstartedServer(t)
	addMacheTools(srv, store)
	require.NoError(t, srv.Start(context.Background()))
	defer srv.Close()

	client := srv.Client()

	// Read a directory as a file
	req := mcp.CallToolRequest{}
	req.Params.Name = "read_file"
	req.Params.Arguments = map[string]any{"path": "pkg"}

	result, err := client.CallTool(context.Background(), req)
	require.NoError(t, err)
	assert.True(t, result.IsError)

	tc, ok := result.Content[0].(mcp.TextContent)
	require.True(t, ok)
	assert.Contains(t, tc.Text, "directory")
}

// ---------------------------------------------------------------------------
// sqlLikeMatch tests
// ---------------------------------------------------------------------------

func TestSqlLikeMatch_CaseInsensitive(t *testing.T) {
	// sqlLikeMatch lowercases both sides — search should be case-insensitive
	assert.True(t, sqlLikeMatch("helper", "Helper"))
	assert.True(t, sqlLikeMatch("HELPER", "Helper"))
	assert.True(t, sqlLikeMatch("%HELP%", "Helper"))
	assert.True(t, sqlLikeMatch("help%", "Helper"))
}

// ---------------------------------------------------------------------------
// lazyGraph --path tests
// ---------------------------------------------------------------------------

func TestLazyGraph_BasePath_DefaultsCWD(t *testing.T) {
	// When basePath is empty, lazyGraph should use "." (CWD behavior)
	lg := &lazyGraph{args: []string{}, basePath: ""}
	// basePath should resolve to "." internally
	assert.Equal(t, ".", lg.resolvedBasePath())
}

func TestLazyGraph_BasePath_UsesProvidedPath(t *testing.T) {
	dir := t.TempDir()
	// Create a Go file so inferDirSchema can detect the project type
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main() {}"), 0o644))

	lg := &lazyGraph{args: []string{}, basePath: dir}
	assert.Equal(t, dir, lg.resolvedBasePath())
}

func TestLazyGraph_BasePath_ProjectConfig(t *testing.T) {
	dir := t.TempDir()
	// Write a .mache.json config in the target dir
	cfg := `{"sources": [{"path": ".", "schema": "go"}]}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(cfg), 0o644))

	lg := &lazyGraph{args: []string{}, basePath: dir}
	lg.init()
	// Should succeed (or at least not error about missing CWD files)
	// The error, if any, should be about the data source in dir, not in CWD
	if lg.err != nil {
		// Acceptable: ingestion errors reference the target dir, not CWD
		assert.Contains(t, lg.err.Error(), dir)
	}
}

func TestLazyGraph_BasePath_InferSchema(t *testing.T) {
	dir := t.TempDir()
	// Create a Go project structure
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main() {}"), 0o644))

	lg := &lazyGraph{args: []string{}, basePath: dir}
	lg.init()
	// Should attempt to infer + ingest from dir, not CWD
	// Even if ingestion fails (no go.mod etc), error should reference dir
	if lg.err != nil {
		assert.NotContains(t, lg.err.Error(), "no such file")
	}
}

// ---------------------------------------------------------------------------
// validateHTTPAddr tests (MCP spec: loopback only)
// ---------------------------------------------------------------------------

func TestValidateHTTPAddr_LocalhostAllowed(t *testing.T) {
	assert.NoError(t, validateHTTPAddr("localhost:7532"))
	assert.NoError(t, validateHTTPAddr("127.0.0.1:7532"))
	assert.NoError(t, validateHTTPAddr("[::1]:7532"))
	assert.NoError(t, validateHTTPAddr("127.0.0.2:9000"))
}

func TestValidateHTTPAddr_AllInterfacesRejected(t *testing.T) {
	assert.Error(t, validateHTTPAddr(":7532"))
	assert.Error(t, validateHTTPAddr("0.0.0.0:7532"))
	assert.Error(t, validateHTTPAddr("[::]:7532"))
}

func TestValidateHTTPAddr_ExternalIPRejected(t *testing.T) {
	assert.Error(t, validateHTTPAddr("192.168.1.100:7532"))
	assert.Error(t, validateHTTPAddr("10.0.0.1:7532"))
}

func TestValidateHTTPAddr_ErrorMessageHelpful(t *testing.T) {
	err := validateHTTPAddr(":9000")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "localhost:9000")
}

// ---------------------------------------------------------------------------
// serve sidecar registration tests
// ---------------------------------------------------------------------------

func TestRegisterServeSidecar_CreatesMetaJSON(t *testing.T) {
	// Override tmpdir so we don't pollute real /tmp/mache
	tmpDir := t.TempDir()
	t.Setenv("TMPDIR", tmpDir)

	meta := registerServeSidecar("/some/project", "mcp-http", "localhost:7532")
	require.NotNil(t, meta)
	defer removeServeSidecar(meta)

	assert.Equal(t, "mcp-http", meta.Type)
	assert.Equal(t, "localhost:7532", meta.Addr)
	assert.Equal(t, "/some/project", meta.Source)
	assert.Equal(t, os.Getpid(), meta.PID)

	// Verify sidecar file exists and is valid JSON
	data, err := os.ReadFile(sidecarPath(meta.MountPoint))
	require.NoError(t, err)

	var loaded MountMetadata
	require.NoError(t, json.Unmarshal(data, &loaded))
	assert.Equal(t, "mcp-http", loaded.Type)
	assert.Equal(t, "localhost:7532", loaded.Addr)
}

func TestRegisterServeSidecar_StdioHasNoAddr(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("TMPDIR", tmpDir)

	meta := registerServeSidecar(".", "mcp-stdio", "")
	require.NotNil(t, meta)
	defer removeServeSidecar(meta)

	assert.Equal(t, "mcp-stdio", meta.Type)
	assert.Equal(t, "", meta.Addr)
}

func TestRemoveServeSidecar_CleansUp(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("TMPDIR", tmpDir)

	meta := registerServeSidecar("/proj", "mcp-http", ":9000")
	require.NotNil(t, meta)

	sidecar := sidecarPath(meta.MountPoint)
	_, err := os.Stat(sidecar)
	require.NoError(t, err, "sidecar should exist before removal")

	removeServeSidecar(meta)

	_, err = os.Stat(sidecar)
	assert.True(t, os.IsNotExist(err), "sidecar should be removed")
}

func TestRemoveServeSidecar_NilSafe(t *testing.T) {
	// Should not panic
	removeServeSidecar(nil)
}

func TestListActiveMounts_IncludesMCPServers(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("TMPDIR", tmpDir)

	meta := registerServeSidecar("/my/project", "mcp-http", "localhost:7532")
	require.NotNil(t, meta)
	defer removeServeSidecar(meta)

	mounts, err := listActiveMounts()
	require.NoError(t, err)
	require.Len(t, mounts, 1)
	assert.Equal(t, "mcp-http", mounts[0].Type)
	assert.Equal(t, "localhost:7532", mounts[0].Addr)
}

// ---------------------------------------------------------------------------
// graphRegistry tests
// ---------------------------------------------------------------------------

func TestGraphRegistry_CachesPerRoot(t *testing.T) {
	registry := newGraphRegistry(".", nil)

	g1 := registry.getOrCreateGraph("/project/a")
	g2 := registry.getOrCreateGraph("/project/a")

	// Same root should return the same lazyGraph instance (pointer equality)
	assert.Same(t, g1, g2)
}

func TestGraphRegistry_DifferentRoots(t *testing.T) {
	registry := newGraphRegistry(".", nil)

	g1 := registry.getOrCreateGraph("/project/a")
	g2 := registry.getOrCreateGraph("/project/b")

	// Different roots should return different graphs
	assert.NotSame(t, g1, g2)
}

func TestGraphRegistry_FallbackToBasePath(t *testing.T) {
	registry := newGraphRegistry("/default/path", nil)

	// Unregistered session should fall back to basePath
	g := registry.graphForSession("unknown-session")
	assert.Equal(t, "/default/path", g.basePath)
}

func TestGraphRegistry_SessionRouting(t *testing.T) {
	registry := newGraphRegistry("/default", nil)

	registry.registerSession("sess-1", "/project/alpha")
	registry.registerSession("sess-2", "/project/beta")

	g1 := registry.graphForSession("sess-1")
	g2 := registry.graphForSession("sess-2")
	gDefault := registry.graphForSession("unknown")

	assert.Equal(t, "/project/alpha", g1.basePath)
	assert.Equal(t, "/project/beta", g2.basePath)
	assert.Equal(t, "/default", gDefault.basePath)
}

func TestGraphRegistry_UnregisterSession(t *testing.T) {
	registry := newGraphRegistry("/default", nil)

	registry.registerSession("sess-1", "/project/alpha")
	g1 := registry.graphForSession("sess-1")
	assert.Equal(t, "/project/alpha", g1.basePath)

	registry.unregisterSession("sess-1")
	// After unregister, session falls back to default
	g2 := registry.graphForSession("sess-1")
	assert.Equal(t, "/default", g2.basePath)

	// But the graph for /project/alpha is still cached (reusable by other sessions)
	g3 := registry.getOrCreateGraph("/project/alpha")
	assert.Same(t, g1, g3)
}

func TestGraphRegistry_WrapHandler_RoutesToSessionGraph(t *testing.T) {
	// Pre-populate stores for two projects and the default
	storeA := graph.NewMemoryStore()
	storeA.AddRoot(&graph.Node{ID: "project-a", Mode: fs.ModeDir, Children: []string{}})
	storeB := graph.NewMemoryStore()
	storeB.AddRoot(&graph.Node{ID: "project-b", Mode: fs.ModeDir, Children: []string{}})
	storeDefault := graph.NewMemoryStore()
	storeDefault.AddRoot(&graph.Node{ID: "default-root", Mode: fs.ModeDir, Children: []string{}})

	registry := newGraphRegistry("/default", nil)
	registry.graphs.Store("/project/a", newTestLazyGraph(storeA, "/project/a"))
	registry.graphs.Store("/project/b", newTestLazyGraph(storeB, "/project/b"))
	registry.graphs.Store("/default", newTestLazyGraph(storeDefault, "/default"))
	registry.registerSession("sess-a", "/project/a")
	registry.registerSession("sess-b", "/project/b")

	handler := registry.wrapHandler(makeGetOverviewHandler)

	// Without session in context, falls back to default graph
	ctx := context.Background()
	result, err := handler(ctx, makeRequest(nil))
	require.NoError(t, err)
	text := resultText(t, result)
	require.False(t, result.IsError, "unexpected error: %s", text)

	// Default graph has "default-root" as top-level
	assert.Contains(t, text, "default-root")
}

// ---------------------------------------------------------------------------
// git HEAD cache isolation tests
// ---------------------------------------------------------------------------

func TestGetGitHead_DirectHash(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("abc123def456789\n"), 0o644))

	got := getGitHead(dir)
	assert.Equal(t, "abc123def456", got) // first 12 chars
}

func TestGetGitHead_RefPointer(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git", "refs", "heads"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "refs", "heads", "main"), []byte("deadbeef12345678\n"), 0o644))

	got := getGitHead(dir)
	assert.Equal(t, "deadbeef1234", got)
}

func TestGetGitHead_NoGitDir(t *testing.T) {
	dir := t.TempDir()
	// No .git directory
	got := getGitHead(dir)
	assert.Empty(t, got)
}

func TestGetGitHead_Worktree(t *testing.T) {
	// Simulate a worktree: .git is a file with "gitdir: <path>"
	mainDir := t.TempDir()
	worktreeDir := t.TempDir()

	// Set up the "real" git dir that the worktree points to
	gitDir := filepath.Join(mainDir, ".git", "worktrees", "wt1")
	require.NoError(t, os.MkdirAll(gitDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("aabbccdd11223344\n"), 0o644))

	// Worktree's .git is a file pointing to the gitdir
	require.NoError(t, os.WriteFile(filepath.Join(worktreeDir, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644))

	got := getGitHead(worktreeDir)
	assert.Equal(t, "aabbccdd1122", got)
}

func TestGetGitHead_PackedRefs(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
	// No loose ref file — only packed-refs
	packedContent := "# pack-refs with: peeled fully-peeled sorted\n" +
		"deadbeefdeadbeef refs/heads/main\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "packed-refs"), []byte(packedContent), 0o644))

	got := getGitHead(dir)
	assert.Equal(t, "deadbeefdead", got)
}

func TestGetGitHead_UnresolvableRef(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o755))
	// HEAD points to a ref that doesn't exist as loose or packed
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/nonexistent\n"), 0o644))

	got := getGitHead(dir)
	assert.Empty(t, got, "unresolvable ref should return empty string, not the ref name")
}

func TestGraphRegistry_GitBranchIsolation(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o755))
	// First "branch" — commit abc123
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("abc123def456\n"), 0o644))

	r := newGraphRegistry(dir, nil)
	g1 := r.getOrCreateGraph(dir)
	// Same branch — should return same instance
	g1again := r.getOrCreateGraph(dir)
	assert.Same(t, g1, g1again, "same commit should return same lazyGraph")

	// Simulate branch switch — new HEAD
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("999aaabbbccc\n"), 0o644))
	g2 := r.getOrCreateGraph(dir)
	assert.NotSame(t, g1, g2, "different commit should return different lazyGraph")
}

func TestGraphRegistry_NoGitStillCachesPerRoot(t *testing.T) {
	// Non-git directory: cache key falls back to rootPath only — still deduplicated
	dir := t.TempDir()
	r := newGraphRegistry(dir, nil)
	g1 := r.getOrCreateGraph(dir)
	g2 := r.getOrCreateGraph(dir)
	assert.Same(t, g1, g2)
}

// ---------------------------------------------------------------------------
// get_communities diagnostic message tests
// ---------------------------------------------------------------------------

func TestGetCommunities_EmptyRefsReturnsMessage(t *testing.T) {
	store := graph.NewMemoryStore()
	handler := makeGetCommunitiesHandler(store)

	result, err := handler(context.Background(), makeRequest(nil))
	require.NoError(t, err)
	require.False(t, result.IsError)

	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &out))
	assert.Contains(t, out, "message", "empty refs should return a diagnostic message")
	communities, ok := out["communities"]
	require.True(t, ok)
	assert.Empty(t, communities)
}

// noRefsGraph is a minimal graph.Graph that does NOT implement refsMapProvider.
// Used to test the unchecked type assertion guard in makeGetCommunitiesHandler.
type noRefsGraph struct {
	graph.Graph
}

func TestGetCommunities_UnsupportedBackend(t *testing.T) {
	// noRefsGraph embeds graph.Graph but does not implement RefsMap().
	// The handler must return an error, not panic.
	g := &noRefsGraph{graph.NewMemoryStore()}
	handler := makeGetCommunitiesHandler(g)

	result, err := handler(context.Background(), makeRequest(nil))
	require.NoError(t, err)
	assert.True(t, result.IsError, "unsupported backend should return an error result")
	assert.Contains(t, resultText(t, result), "cross-reference")
}

// ---------------------------------------------------------------------------
// get_diagram handler tests
// ---------------------------------------------------------------------------

// buildDiagramTestGraph creates a MemoryStore with two distinct communities
// connected by a bridge token, suitable for testing get_diagram.
//
// Community 1: nodes a1, a2, a3 all reference "alpha"
// Community 2: nodes b1, b2, b3 all reference "beta"
// Bridge: a1 and b1 both reference "bridge"
func buildDiagramTestGraph(t *testing.T) *graph.MemoryStore {
	t.Helper()
	store := graph.NewMemoryStore()

	for _, id := range []string{"a1", "a2", "a3"} {
		store.AddNode(&graph.Node{ID: id, Mode: fs.ModeDir})
		require.NoError(t, store.AddRef("alpha", id))
	}
	for _, id := range []string{"b1", "b2", "b3"} {
		store.AddNode(&graph.Node{ID: id, Mode: fs.ModeDir})
		require.NoError(t, store.AddRef("beta", id))
	}
	require.NoError(t, store.AddRef("bridge", "a1"))
	require.NoError(t, store.AddRef("bridge", "b1"))

	return store
}

func TestGetDiagram_BasicMermaid(t *testing.T) {
	store := buildDiagramTestGraph(t)
	handler := makeGetDiagramHandler(store)

	result, err := handler(context.Background(), makeRequest(nil))
	require.NoError(t, err)
	require.False(t, result.IsError, "should succeed with refs data")

	text := resultText(t, result)
	assert.Contains(t, text, "graph TD", "default layout should be TD")
	assert.Contains(t, text, "subgraph", "multi-member classes should produce subgraphs")
}

func TestGetDiagram_LayoutOverride(t *testing.T) {
	store := buildDiagramTestGraph(t)
	handler := makeGetDiagramHandler(store)

	result, err := handler(context.Background(), makeRequest(map[string]any{"layout": "LR"}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	text := resultText(t, result)
	assert.Contains(t, text, "graph LR", "layout override should be respected")
}

func TestGetDiagram_InvalidLayout(t *testing.T) {
	store := buildDiagramTestGraph(t)
	handler := makeGetDiagramHandler(store)

	result, err := handler(context.Background(), makeRequest(map[string]any{"layout": "DIAGONAL"}))
	require.NoError(t, err)
	assert.True(t, result.IsError, "invalid layout should return error")
	assert.Contains(t, resultText(t, result), "invalid layout")
}

func TestGetDiagram_NoRefs(t *testing.T) {
	store := graph.NewMemoryStore()
	handler := makeGetDiagramHandler(store)

	result, err := handler(context.Background(), makeRequest(nil))
	require.NoError(t, err)
	assert.True(t, result.IsError, "empty refs should return error")
	assert.Contains(t, resultText(t, result), "cross-references")
}

func TestGetDiagram_UnsupportedBackend(t *testing.T) {
	g := &noRefsGraph{graph.NewMemoryStore()}
	handler := makeGetDiagramHandler(g)

	result, err := handler(context.Background(), makeRequest(nil))
	require.NoError(t, err)
	assert.True(t, result.IsError, "unsupported backend should return error")
	assert.Contains(t, resultText(t, result), "cross-reference")
}

func TestGetDiagram_SingleCommunity(t *testing.T) {
	// Two nodes sharing one token form a single community -- valid diagram, no edges
	store := graph.NewMemoryStore()
	store.AddNode(&graph.Node{ID: "x1", Mode: fs.ModeDir})
	store.AddNode(&graph.Node{ID: "x2", Mode: fs.ModeDir})
	require.NoError(t, store.AddRef("solo", "x1"))
	require.NoError(t, store.AddRef("solo", "x2"))

	handler := makeGetDiagramHandler(store)
	result, err := handler(context.Background(), makeRequest(nil))
	require.NoError(t, err)
	require.False(t, result.IsError, "single community is valid")

	text := resultText(t, result)
	assert.Contains(t, text, "graph TD")
	assert.NotContains(t, text, "-->", "single community should have no cross-class edges")
}

func TestGetDiagram_CaseInsensitiveLayout(t *testing.T) {
	store := buildDiagramTestGraph(t)
	handler := makeGetDiagramHandler(store)

	result, err := handler(context.Background(), makeRequest(map[string]any{"layout": "lr"}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	assert.Contains(t, resultText(t, result), "graph LR", "lowercase layout should be normalized")
}

// schemaGraph wraps a MemoryStore and adds schemaProvider support.
type schemaGraph struct {
	*graph.MemoryStore
	schema *api.Topology
}

func (sg *schemaGraph) Schema() *api.Topology { return sg.schema }

func TestGetDiagram_NameResolvesSchemaLayout(t *testing.T) {
	store := buildDiagramTestGraph(t)
	sg := &schemaGraph{
		MemoryStore: store,
		schema: &api.Topology{
			Version: "v1",
			Diagrams: map[string]api.DiagramDef{
				"architecture": {Layout: "LR"},
			},
		},
	}

	handler := makeGetDiagramHandler(sg)
	result, err := handler(context.Background(), makeRequest(map[string]any{"name": "architecture"}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	assert.Contains(t, resultText(t, result), "graph LR", "name should resolve to schema-defined layout")
}

func TestGetDiagram_NameNotInSchema(t *testing.T) {
	store := buildDiagramTestGraph(t)
	sg := &schemaGraph{
		MemoryStore: store,
		schema: &api.Topology{
			Version: "v1",
			Diagrams: map[string]api.DiagramDef{
				"deps": {Layout: "TD"},
			},
		},
	}

	handler := makeGetDiagramHandler(sg)
	result, err := handler(context.Background(), makeRequest(map[string]any{"name": "missing"}))
	require.NoError(t, err)
	assert.True(t, result.IsError, "undefined diagram name should return error")
	assert.Contains(t, resultText(t, result), "not defined")
}

func TestGetDiagram_LayoutOverridesName(t *testing.T) {
	store := buildDiagramTestGraph(t)
	sg := &schemaGraph{
		MemoryStore: store,
		schema: &api.Topology{
			Version: "v1",
			Diagrams: map[string]api.DiagramDef{
				"architecture": {Layout: "LR"},
			},
		},
	}

	// Explicit layout should take precedence over schema definition
	handler := makeGetDiagramHandler(sg)
	result, err := handler(context.Background(), makeRequest(map[string]any{
		"name":   "architecture",
		"layout": "BT",
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	assert.Contains(t, resultText(t, result), "graph BT", "explicit layout should override schema")
}

func TestGetDiagram_SystemNameAlwaysAllowed(t *testing.T) {
	store := buildDiagramTestGraph(t)
	sg := &schemaGraph{
		MemoryStore: store,
		schema: &api.Topology{
			Version: "v1",
			Diagrams: map[string]api.DiagramDef{
				"custom": {Layout: "RL"},
			},
		},
	}

	// "system" should work even when not explicitly in the schema
	handler := makeGetDiagramHandler(sg)
	result, err := handler(context.Background(), makeRequest(map[string]any{"name": "system"}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	assert.Contains(t, resultText(t, result), "graph TD", "system should default to TD")
}

// ---------------------------------------------------------------------------
// find_callees generic name warning tests
// ---------------------------------------------------------------------------

func TestFindCallees_GenericNameWarning(t *testing.T) {
	store := graph.NewMemoryStore()
	// Construct node with a "source" child
	store.AddRoot(&graph.Node{
		ID:       "pkg/svc",
		Mode:     fs.ModeDir,
		Children: []string{"pkg/svc/source"},
		Properties: map[string][]byte{
			"lang": []byte("go"),
		},
	})
	store.AddNode(&graph.Node{
		ID:   "pkg/svc/source",
		Mode: 0,
		Data: []byte(`func run() { s := obj.String() }`),
	})
	// Definition for the generic name "String" at some node
	require.NoError(t, store.AddDef("String", "pkg/other/String"))
	store.AddNode(&graph.Node{ID: "pkg/other/String", Mode: fs.ModeDir, Children: []string{}})

	// Extractor returns a call to "String" (bare, generic name)
	store.SetCallExtractor(func(content []byte, path, lang string) ([]graph.QualifiedCall, error) {
		return []graph.QualifiedCall{{Token: "String"}}, nil
	})

	handler := makeFindCalleesHandler(store)
	result, err := handler(context.Background(), makeRequest(map[string]any{"path": "pkg/svc"}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &out))
	assert.Contains(t, out, "callees")
	assert.Contains(t, out, "warnings", "generic names should produce a warnings field")
	warnings := out["warnings"].([]any)
	assert.NotEmpty(t, warnings)
}

// ---------------------------------------------------------------------------
// search unchecked assertion tests
// ---------------------------------------------------------------------------

// noQueryGraph is a minimal graph.Graph that does NOT implement refsQuerier.
type noQueryGraph struct {
	graph.Graph
}

func TestSearch_NonSQLiteBackendReturnsError(t *testing.T) {
	g := &noQueryGraph{graph.NewMemoryStore()}
	handler := makeSearchHandler(g)

	result, err := handler(context.Background(), makeRequest(map[string]any{"pattern": "Foo"}))
	require.NoError(t, err)
	assert.True(t, result.IsError, "non-SQLite backend should return an error, not panic")
	assert.Contains(t, resultText(t, result), "role=definition")
}

func TestRootURIToPath(t *testing.T) {
	tests := []struct {
		uri  string
		want string
	}{
		{"file:///home/user/project", "/home/user/project"},
		{"file:///Users/james/code", "/Users/james/code"},
		{"file:///tmp/test/../real", "/tmp/real"},
		{"https://example.com", ""},
		{"not-a-uri", ""},
	}
	for _, tt := range tests {
		t.Run(tt.uri, func(t *testing.T) {
			assert.Equal(t, tt.want, rootURIToPath(tt.uri))
		})
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// addMacheTools registers all mache MCP tools on a mcptest server (with search).
func addMacheTools(srv *mcptest.Server, store *graph.MemoryStore) {
	srv.AddTool(
		mcp.NewTool("list_directory",
			mcp.WithDescription("List children of a directory node."),
			mcp.WithString("path", mcp.Description("Directory path")),
		),
		makeListDirHandler(store),
	)
	srv.AddTool(
		mcp.NewTool("read_file",
			mcp.WithDescription("Read file content."),
			mcp.WithString("path", mcp.Required(), mcp.Description("File path")),
		),
		makeReadFileHandler(store),
	)
	srv.AddTool(
		mcp.NewTool("find_callers",
			mcp.WithDescription("Find callers."),
			mcp.WithString("token", mcp.Required(), mcp.Description("Token")),
		),
		makeFindCallersHandler(store),
	)
	srv.AddTool(
		mcp.NewTool("find_callees",
			mcp.WithDescription("Find callees."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Path")),
		),
		makeFindCalleesHandler(store),
	)
	srv.AddTool(
		mcp.NewTool("search",
			mcp.WithDescription("Search for symbols."),
			mcp.WithString("pattern", mcp.Required(), mcp.Description("Pattern")),
			mcp.WithNumber("limit", mcp.Description("Max results")),
		),
		makeSearchHandler(store),
	)
	srv.AddTool(
		mcp.NewTool("get_communities",
			mcp.WithDescription("Detect communities."),
			mcp.WithNumber("min_size", mcp.Description("Min size")),
		),
		makeGetCommunitiesHandler(store),
	)
}

// newTestLazyGraph creates a lazyGraph that is already initialized with the given graph.
// This avoids triggering real filesystem detection in tests.
func newTestLazyGraph(g graph.Graph, basePath string) *lazyGraph {
	lg := &lazyGraph{inner: g, basePath: basePath}
	lg.once.Do(func() {}) // mark as initialized
	return lg
}
