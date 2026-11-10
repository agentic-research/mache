package mcpserve

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/lltest"
)

// get_overview's ref_tokens is counted in SQL rather than as len(RefsMap()),
// which rendered a path for every reference only to take the length
// (mache-a6c283). On a real parse by the pinned leyline it is the same number.
func TestGetOverview_RefTokensIsTheRefsMapLength(t *testing.T) {
	ll := lltest.ResolveBinaryOrSkip(t)
	src := t.TempDir()
	for p, body := range map[string]string{
		"main.go":   "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(helper()); run() }\n\nfunc run() { _ = helper() }\n",
		"util.go":   "package main\n\nimport \"strings\"\n\nfunc helper() string { return strings.ToUpper(\"x\") }\n",
		"README.md": "# demo\n\nCall `helper` from `main`.\n",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(src, p), []byte(body), 0o644))
	}
	dbPath := filepath.Join(t.TempDir(), "out.db")
	out, err := exec.Command(ll.Path, "parse", src, "-o", dbPath).CombinedOutput()
	require.NoError(t, err, "%s", out)
	g, _, cleanup, err := openDBGraph(dbPath, &api.Topology{Version: api.SchemaVersion}, nil)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	want := len(g.(graph.RefsMapper).RefsMap())
	require.Positive(t, want, "the fixture must have references to count")
	res, err := makeGetOverviewHandler(g)(context.Background(), mcp.CallToolRequest{})
	require.NoError(t, err)
	var ov struct {
		RefTokens int `json:"ref_tokens"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcp.TextContent).Text), &ov))
	assert.Equal(t, want, ov.RefTokens)
}
