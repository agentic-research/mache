package cmd

import (
	"context"
	"encoding/json"
	"io/fs"
	"testing"

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
	assert.Equal(t, "[]", resultText(t, result))
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
// registerMCPTools tests
// ---------------------------------------------------------------------------

func TestRegisterMCPTools_WithQueryRefs(t *testing.T) {
	store := buildTestGraph(t)
	require.NoError(t, store.InitRefsDB())
	defer func() { _ = store.Close() }()
	require.NoError(t, store.FlushRefs())

	s := server.NewMCPServer("test", "1.0.0", server.WithToolCapabilities(false))
	registerMCPTools(s, store)

	// List tools via mcptest roundtrip
	srv := mcptest.NewUnstartedServer(t)
	addMacheTools(srv, store)
	require.NoError(t, srv.Start(context.Background()))
	defer srv.Close()

	listResult, err := srv.Client().ListTools(context.Background(), mcp.ListToolsRequest{})
	require.NoError(t, err)

	toolNames := map[string]bool{}
	for _, tool := range listResult.Tools {
		toolNames[tool.Name] = true
	}

	assert.True(t, toolNames["list_directory"])
	assert.True(t, toolNames["read_file"])
	assert.True(t, toolNames["find_callers"])
	assert.True(t, toolNames["find_callees"])
	assert.True(t, toolNames["search"], "search should be registered when backend supports QueryRefs")
	assert.True(t, toolNames["get_communities"], "get_communities should be registered when backend supports RefsMap")
}

func TestRegisterMCPTools_WithoutQueryRefs(t *testing.T) {
	// Use a mock graph that does NOT implement refsQuerier
	mock := &mockGraph{}

	s := server.NewMCPServer("test", "1.0.0", server.WithToolCapabilities(false))
	registerMCPTools(s, mock)

	srv := mcptest.NewUnstartedServer(t)
	addMacheToolsFromGraph(srv, mock)
	require.NoError(t, srv.Start(context.Background()))
	defer srv.Close()

	listResult, err := srv.Client().ListTools(context.Background(), mcp.ListToolsRequest{})
	require.NoError(t, err)

	toolNames := map[string]bool{}
	for _, tool := range listResult.Tools {
		toolNames[tool.Name] = true
	}

	assert.True(t, toolNames["list_directory"])
	assert.True(t, toolNames["read_file"])
	assert.True(t, toolNames["find_callers"])
	assert.True(t, toolNames["find_callees"])
	assert.False(t, toolNames["search"], "search should NOT be registered without QueryRefs")
	assert.False(t, toolNames["get_communities"], "get_communities should NOT be registered without RefsMap")
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

// addMacheToolsFromGraph registers tools using a generic Graph (no search).
func addMacheToolsFromGraph(srv *mcptest.Server, g graph.Graph) {
	srv.AddTool(
		mcp.NewTool("list_directory",
			mcp.WithDescription("List children."),
			mcp.WithString("path", mcp.Description("Path")),
		),
		makeListDirHandler(g),
	)
	srv.AddTool(
		mcp.NewTool("read_file",
			mcp.WithDescription("Read file."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Path")),
		),
		makeReadFileHandler(g),
	)
	srv.AddTool(
		mcp.NewTool("find_callers",
			mcp.WithDescription("Find callers."),
			mcp.WithString("token", mcp.Required(), mcp.Description("Token")),
		),
		makeFindCallersHandler(g),
	)
	srv.AddTool(
		mcp.NewTool("find_callees",
			mcp.WithDescription("Find callees."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Path")),
		),
		makeFindCalleesHandler(g),
	)
}

// mockGraph is a minimal Graph implementation that does NOT support QueryRefs.
type mockGraph struct{}

func (m *mockGraph) GetNode(id string) (*graph.Node, error) {
	if id == "" || id == "/" {
		return &graph.Node{ID: "", Mode: fs.ModeDir}, nil
	}
	return nil, graph.ErrNotFound
}

func (m *mockGraph) ListChildren(id string) ([]string, error) {
	return nil, nil
}

func (m *mockGraph) ReadContent(id string, buf []byte, offset int64) (int, error) {
	return 0, graph.ErrNotFound
}

func (m *mockGraph) GetCallers(token string) ([]*graph.Node, error) {
	return nil, nil
}

func (m *mockGraph) GetCallees(id string) ([]*graph.Node, error) {
	return nil, nil
}

func (m *mockGraph) Invalidate(id string) {}

func (m *mockGraph) Act(id, action, payload string) (*graph.ActionResult, error) {
	return nil, graph.ErrActNotSupported
}
