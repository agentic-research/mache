package mcpserve

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/lltest"
)

// callJSON runs a tool handler with no arguments and decodes its answer.
func callJSON(t *testing.T, h server.ToolHandlerFunc, out any) {
	t.Helper()
	res, err := h(context.Background(), mcp.CallToolRequest{})
	require.NoError(t, err)
	require.False(t, res.IsError, "%v", res.Content)
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcp.TextContent).Text), out))
}

// On a leyline parse a source file is a directory of syntax nodes, so
// counting the graph's file nodes counted syntax leaves: get_architecture
// reported 11,015 files for mache's 1,067 and no test files, and get_overview
// reported 0 files (mache-7fed6a). Both now count the source tree, `_tree`,
// against a real parse by the pinned leyline.
func TestArchitectureAndOverview_CountTheSourceTree(t *testing.T) {
	src := t.TempDir()
	files := map[string]string{
		"main.go":         "package main\n\nfunc main() {}\n",
		"main_test.go":    "package main\n\nimport \"testing\"\n\nfunc TestMain(t *testing.T) {}\n",
		"pkg/a.go":        "package pkg\n\nfunc A() int { return 1 }\n",
		"pkg/a_test.go":   "package pkg\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n",
		"pkg/sub/b.go":    "package sub\n\nfunc B() {}\n",
		"pkg/sub/doc.txt": "not source\n",
	}
	for p, body := range files {
		require.NoError(t, os.MkdirAll(filepath.Join(src, filepath.Dir(p)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(src, p), []byte(body), 0o644))
	}
	ll := lltest.ResolveBinaryOrSkip(t)
	dbPath := filepath.Join(t.TempDir(), "out.db")
	out, err := exec.Command(ll.Path, "parse", src, "-o", dbPath).CombinedOutput()
	require.NoError(t, err, "%s", out)
	g, _, cleanup, err := openDBGraph(dbPath, &api.Topology{Version: api.SchemaVersion}, nil)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	var arch struct {
		FileCount int            `json:"file_count"`
		TestFiles []string       `json:"test_files"`
		TopLevel  map[string]int `json:"top_level_breakdown"`
	}
	callJSON(t, makeGetArchitectureHandler(g), &arch)
	assert.Equal(t, len(files), arch.FileCount, "file_count must be the source tree's files")
	assert.ElementsMatch(t, []string{"main_test.go", "pkg/a_test.go"}, arch.TestFiles)
	assert.Equal(t, map[string]int{"main.go": 1, "main_test.go": 1, "pkg": 4}, arch.TopLevel)

	var ov struct {
		TotalFiles int `json:"total_files"`
		TotalDirs  int `json:"total_dirs"`
	}
	callJSON(t, makeGetOverviewHandler(g), &ov)
	assert.Equal(t, len(files), ov.TotalFiles, "total_files must be the source tree's files")
	assert.Equal(t, 2, ov.TotalDirs, "pkg and pkg/sub")
}
