package mcpharness

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/leyline"
	"github.com/agentic-research/mache/internal/procscan"
)

// toyServer has two tools: echo, and roots, which asks the calling client
// for its roots the way mache asks for a workspace.
func toyServer() *server.MCPServer {
	s := server.NewMCPServer("toy", "1", server.WithToolCapabilities(false))
	s.AddTool(mcp.NewTool("echo", mcp.WithString("say")), func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText(r.GetString("say", "")), nil
	})
	s.AddTool(mcp.NewTool("roots"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		rs, ok := server.ClientSessionFromContext(ctx).(server.SessionWithRoots)
		if !ok {
			return mcp.NewToolResultError("no roots support"), nil
		}
		res, err := rs.ListRoots(ctx, mcp.ListRootsRequest{})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		var uris []string
		for _, r := range res.Roots {
			uris = append(uris, r.URI)
		}
		return mcp.NewToolResultText(strings.Join(uris, ",")), nil
	})
	return s
}

func TestInProcess_CallsAndAnswersRoots(t *testing.T) {
	s := InProcess(t, toyServer(), Behavior{Roots: []string{"/w/a"}})
	assert.ElementsMatch(t, []string{"echo", "roots"}, s.Tools())
	assert.Equal(t, "hi", s.Text("echo", map[string]any{"say": "hi"}))
	assert.Equal(t, "file:///w/a", s.Text("roots", nil))
}

// Over HTTP a server request outside the POST reaches the client only on the
// GET stream, which Listen holds open for the whole session.
func TestServed_ListenCarriesServerRequests(t *testing.T) {
	h := server.NewStreamableHTTPServer(toyServer())
	s := Served(t, h, "/mcp", Behavior{Roots: []string{"/w/b"}, Listen: true})
	assert.Equal(t, "hi", s.Text("echo", map[string]any{"say": "hi"}))
	assert.Equal(t, "file:///w/b", s.Text("roots", nil))
}

// HTTP reaches an endpoint by URL and carries the ?project= token in it, the
// way `mache init` writes a client's config.
func TestHTTP_CarriesTheProjectToken(t *testing.T) {
	var got string
	h := server.NewStreamableHTTPServer(toyServer(), server.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context {
		if q := r.URL.Query().Get("project"); q != "" {
			got = q
		}
		return ctx
	}))
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	s := HTTP(t, ts.URL+"/mcp", Behavior{Project: "tok123"})
	assert.Equal(t, "hi", s.Text("echo", map[string]any{"say": "hi"}))
	assert.Equal(t, "tok123", got)
}

// The stdio transport runs this test binary as the server.
func TestStdio_RunsAServerProcess(t *testing.T) {
	s := Stdio(t, os.Args[0], append(os.Environ(), "MCPHARNESS_TOY_STDIO=1"),
		[]string{"-test.run=^TestHelperToyStdioServer$"}, Behavior{Roots: []string{"/w/c"}})
	assert.Equal(t, "hi", s.Text("echo", map[string]any{"say": "hi"}))
	assert.Equal(t, "file:///w/c", s.Text("roots", nil))
}

// TestHelperToyStdioServer is the stdio server process, not a test.
func TestHelperToyStdioServer(t *testing.T) {
	if os.Getenv("MCPHARNESS_TOY_STDIO") != "1" {
		t.Skip("helper process for TestStdio_RunsAServerProcess")
	}
	require.NoError(t, server.ServeStdio(toyServer()))
	os.Exit(0)
}

func TestIsolatedHome_MovesMacheHomeAndKeepsTheBinaryCache(t *testing.T) {
	home := IsolatedHome(t)
	assert.Equal(t, home, os.Getenv("MACHE_HOME"))
	assert.DirExists(t, home)
}

// A daemon started under an isolated home does not outlive the test that
// started it. The inner test spawns one the way a serve-mode graph does; once
// it has ended, the daemon is gone and the home with it (mache-252d27).
// Removing IsolatedHome's StopManaged cleanup fails this test on both counts.
func TestIsolatedHome_StopsTheDaemonStartedUnderIt(t *testing.T) {
	if _, err := leyline.ResolveBinary(false); err != nil {
		t.Skipf("pinned leyline unavailable: %v", err)
	}
	var home string
	t.Run("a test that starts a daemon", func(t *testing.T) {
		home = IsolatedHome(t)
		_, err := leyline.DiscoverOrStart()
		require.NoError(t, err)
		running, err := procscan.Referencing(home)
		require.NoError(t, err)
		require.NotEmpty(t, running, "no process names the isolated home: the daemon did not start under it")
	})
	survivors, err := procscan.Referencing(home)
	require.NoError(t, err)
	assert.Empty(t, survivors, "the daemon outlived the test that started it")
	assert.NoDirExists(t, home)
}

// The core API returns errors for a tool to report, where the test API fails
// the test.
func TestConnect_ReturnsErrorsAndConnects(t *testing.T) {
	ctx := context.Background()

	c, err := ConnectInProcess(ctx, toyServer(), Behavior{})
	require.NoError(t, err)
	tools, err := c.Tools(ctx)
	require.NoError(t, err)
	assert.Len(t, tools, 2)
	text, isError, err := c.Call(ctx, "echo", map[string]any{"say": "core"})
	require.NoError(t, err)
	assert.False(t, isError)
	assert.Equal(t, "core", text)
	require.NoError(t, c.Close())

	ts := httptest.NewServer(server.NewStreamableHTTPServer(toyServer()))
	url := ts.URL + "/mcp"
	ts.Close()
	_, err = ConnectHTTP(ctx, url, Behavior{})
	assert.Error(t, err, "a server that is not there")

	_, err = ConnectStdio(ctx, "/nonexistent/mcp-server", nil, nil, Behavior{})
	assert.Error(t, err, "a binary that is not there")
}

func TestParsePlan(t *testing.T) {
	plan, err := ParsePlan([]byte(`[{"tool":"echo","args":{"say":"x"}},{"tool":"roots"}]`))
	require.NoError(t, err)
	assert.Equal(t, []Call{{Tool: "echo", Args: map[string]any{"say": "x"}}, {Tool: "roots"}}, plan)
	_, err = ParsePlan([]byte(`[]`))
	assert.ErrorContains(t, err, "plan is empty")
	_, err = ParsePlan([]byte(`{`))
	assert.Error(t, err)
}
