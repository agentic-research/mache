// Package mcpharness drives an MCP server the way a real client does: with
// mcp-go's own client, over any transport, as a client that does or does not
// answer roots/list or hold the standalone GET stream.
//
// Tests and tools that talk to mache over MCP use it instead of each writing
// a client. A hand-rolled one decides what it supports by accident: one that
// never opened the GET stream failed every roots-resolved call over HTTP, and
// the failure was put down to the test client rather than found to be the
// bug it was (mache-ab00c9). What varies between real clients is injected
// here (the transport and the client's behaviour); everything else is the
// real client.
package mcpharness

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/machehome"
)

// Behavior is what a client does that real clients differ on.
type Behavior struct {
	// Roots are the workspace roots the client answers roots/list with, as
	// directory paths. None means the client declares no roots capability.
	Roots []string
	// Listen holds the standalone GET stream open (HTTP only), the channel a
	// server request outside any POST reaches the client on.
	Listen bool
	// Project is the ?project= token appended to an HTTP URL, as `mache
	// init` writes it into a client's config.
	Project string
	// Protocol is the MCP revision the client offers, Legacy when empty.
	// Revisions differ in kind, not just detail: 2026-07-28 removed the
	// standalone GET stream and server-initiated requests, so a client on it
	// can never be asked roots/list at all.
	Protocol string
}

// The revisions a matrix should cover: the last one that has sessions,
// server requests and the GET stream, and the stateless one after it.
const (
	Legacy = mcp.ProtocolVersion20251125
	Modern = mcp.ProtocolVersion20260728
)

// Session is one initialized client connection.
type Session struct {
	t testing.TB
	c *client.Client
}

// rootsHandler answers roots/list with the Behavior's roots.
type rootsHandler struct{ roots []string }

func (h rootsHandler) ListRoots(context.Context, mcp.ListRootsRequest) (*mcp.ListRootsResult, error) {
	out := &mcp.ListRootsResult{Roots: []mcp.Root{}}
	for _, p := range h.roots {
		out.Roots = append(out.Roots, mcp.Root{URI: (&url.URL{Scheme: "file", Path: p}).String(), Name: filepath.Base(p)})
	}
	return out, nil
}

func clientOptions(b Behavior) []client.ClientOption {
	if len(b.Roots) == 0 {
		return nil
	}
	return []client.ClientOption{client.WithRootsHandler(rootsHandler{b.Roots})}
}

// InProcess connects to s in process.
func InProcess(t testing.TB, s *server.MCPServer, b Behavior) *Session {
	t.Helper()
	c, err := client.NewInProcessClientWithOptions(s, clientOptions(b)...)
	require.NoError(t, err)
	return start(t, c, b)
}

// HTTP connects to the Streamable HTTP endpoint at endpoint.
func HTTP(t testing.TB, endpoint string, b Behavior) *Session {
	t.Helper()
	if b.Project != "" {
		u, err := url.Parse(endpoint)
		require.NoError(t, err)
		q := u.Query()
		q.Set("project", b.Project)
		u.RawQuery = q.Encode()
		endpoint = u.String()
	}
	var opts []transport.StreamableHTTPCOption
	if b.Listen {
		// Deprecated because 2026-07-28 has no GET stream; a legacy client
		// holding one is exactly what this models.
		opts = append(opts, transport.WithContinuousListening()) //nolint:staticcheck // models a legacy client on purpose
	}
	tr, err := transport.NewStreamableHTTP(endpoint, opts...)
	require.NoError(t, err)
	return start(t, client.NewClient(tr, clientOptions(b)...), b)
}

// Served serves h on a test server and connects to it at path over HTTP.
func Served(t testing.TB, h http.Handler, path string, b Behavior) *Session {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return HTTP(t, ts.URL+path, b)
}

// Stdio runs bin with args and env and connects over its stdin/stdout.
func Stdio(t testing.TB, bin string, env, args []string, b Behavior) *Session {
	t.Helper()
	return start(t, client.NewClient(transport.NewStdio(bin, env, args...), clientOptions(b)...), b)
}

func start(t testing.TB, c *client.Client, b Behavior) *Session {
	t.Helper()
	// The client's background work (the GET listener among it) runs on the
	// context Start is given, so it must live as long as the session, not
	// end with this function.
	life, end := context.WithCancel(context.Background())
	require.NoError(t, c.Start(life))
	t.Cleanup(func() { _ = c.Close(); end() })
	ctx, cancel := context.WithTimeout(life, 2*time.Minute)
	defer cancel()
	req := mcp.InitializeRequest{}
	req.Params.ProtocolVersion = b.Protocol
	if req.Params.ProtocolVersion == "" {
		req.Params.ProtocolVersion = Legacy
	}
	req.Params.ClientInfo = mcp.Implementation{Name: "mcpharness", Version: "1"}
	if len(b.Roots) > 0 {
		req.Params.Capabilities.Roots = &struct {
			ListChanged bool `json:"listChanged,omitempty"`
		}{}
	}
	_, err := c.Initialize(ctx, req)
	require.NoError(t, err)
	return &Session{t: t, c: c}
}

// Call calls tool and returns its text and whether the tool reported an
// error. A protocol or transport failure fails the test.
func (s *Session) Call(tool string, args map[string]any) (text string, isError bool) {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	req := mcp.CallToolRequest{}
	req.Params.Name = tool
	req.Params.Arguments = args
	res, err := s.c.CallTool(ctx, req)
	require.NoError(s.t, err, "call %s", tool)
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			text += tc.Text
		}
	}
	return text, res.IsError
}

// Text calls tool and requires it to succeed.
func (s *Session) Text(tool string, args map[string]any) string {
	s.t.Helper()
	text, isError := s.Call(tool, args)
	require.False(s.t, isError, "%s reported an error: %s", tool, text)
	return text
}

// Tools lists the tool names the server offers.
func (s *Session) Tools() []string {
	s.t.Helper()
	res, err := s.c.ListTools(context.Background(), mcp.ListToolsRequest{})
	require.NoError(s.t, err)
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// IsolatedHome points MACHE_HOME at a fresh directory for the rest of the
// test, with its bin/ linked to the real pinned-binary cache, so a server
// under test keeps its state there while resolving the pinned leyline as an
// install would. The directory is under /tmp because the managed daemon binds
// a socket beneath it and macOS caps socket paths at 104 bytes.
func IsolatedHome(t testing.TB) string {
	t.Helper()
	bin, err := machehome.BinPath("")
	require.NoError(t, err)
	home, err := os.MkdirTemp("/tmp", "mh")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	if _, err := os.Stat(bin); err == nil {
		require.NoError(t, os.Symlink(bin, filepath.Join(home, "bin")))
	}
	t.Setenv(machehome.Env, home)
	return home
}
