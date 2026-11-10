package mcpharness

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/machehome"
)

// The test-side API: each constructor connects or fails the test, and closes
// the connection when the test ends.

// callTimeout bounds one tool call in a test.
const callTimeout = 2 * time.Minute

// Session is a Conn bound to a test.
type Session struct {
	t testing.TB
	*Conn
}

func bind(t testing.TB, c *Conn, err error) *Session {
	t.Helper()
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return &Session{t: t, Conn: c}
}

func handshake() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), callTimeout)
}

// InProcess connects to s in process.
func InProcess(t testing.TB, s *server.MCPServer, b Behavior) *Session {
	t.Helper()
	ctx, cancel := handshake()
	defer cancel()
	c, err := ConnectInProcess(ctx, s, b)
	return bind(t, c, err)
}

// HTTP connects to the Streamable HTTP endpoint at endpoint.
func HTTP(t testing.TB, endpoint string, b Behavior) *Session {
	t.Helper()
	ctx, cancel := handshake()
	defer cancel()
	c, err := ConnectHTTP(ctx, endpoint, b)
	return bind(t, c, err)
}

// Served serves h on a test server and connects to it at path over HTTP.
func Served(t testing.TB, h http.Handler, path string, b Behavior) *Session {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return HTTP(t, ts.URL+path, b)
}

// Stdio runs bin with args and env and connects over its stdin and stdout.
func Stdio(t testing.TB, bin string, env, args []string, b Behavior) *Session {
	t.Helper()
	ctx, cancel := handshake()
	defer cancel()
	c, err := ConnectStdio(ctx, bin, env, args, b)
	return bind(t, c, err)
}

// Call calls tool and returns its text and whether the tool reported an
// error. A protocol or transport failure fails the test.
func (s *Session) Call(tool string, args map[string]any) (text string, isError bool) {
	s.t.Helper()
	ctx, cancel := handshake()
	defer cancel()
	text, isError, err := s.Conn.Call(ctx, tool, args)
	require.NoError(s.t, err, "call %s", tool)
	return text, isError
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
	ctx, cancel := handshake()
	defer cancel()
	tools, err := s.Conn.Tools(ctx)
	require.NoError(s.t, err)
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
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
