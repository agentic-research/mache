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
