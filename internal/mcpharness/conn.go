// Package mcpharness drives an MCP server the way a real client does: with
// mcp-go's own client, over any transport, as a client that does or does not
// answer roots/list or hold the standalone GET stream.
//
// Everything that talks to mache over MCP uses it instead of writing its own
// client: the tests (through testing.go's wrappers), tools/release-parity and
// tools/mcp-probe. A hand-rolled client decides what it supports by accident:
// one that never opened the GET stream failed every roots-resolved call over
// HTTP, and the failure was put down to the client rather than found to be
// the bug it was (mache-ab00c9). What varies between real clients is injected
// here (the transport and the client's behaviour); everything else is the
// real client.
package mcpharness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
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

// Conn is one initialized client connection.
type Conn struct {
	c   *client.Client
	end context.CancelFunc
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

// ConnectInProcess connects to s in process.
func ConnectInProcess(ctx context.Context, s *server.MCPServer, b Behavior) (*Conn, error) {
	c, err := client.NewInProcessClientWithOptions(s, clientOptions(b)...)
	if err != nil {
		return nil, err
	}
	return connect(ctx, c, b)
}

// ConnectHTTP connects to the Streamable HTTP endpoint at endpoint.
func ConnectHTTP(ctx context.Context, endpoint string, b Behavior) (*Conn, error) {
	if b.Project != "" {
		u, err := url.Parse(endpoint)
		if err != nil {
			return nil, err
		}
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
	if err != nil {
		return nil, err
	}
	return connect(ctx, client.NewClient(tr, clientOptions(b)...), b)
}

// ConnectStdio runs bin with args and env and connects over its stdin and
// stdout.
func ConnectStdio(ctx context.Context, bin string, env, args []string, b Behavior) (*Conn, error) {
	return connect(ctx, client.NewClient(transport.NewStdio(bin, env, args...), clientOptions(b)...), b)
}

// connect starts c and initializes it. ctx bounds the handshake only.
func connect(ctx context.Context, c *client.Client, b Behavior) (*Conn, error) {
	// The client's background work (the GET listener among it) runs on the
	// context Start is given, so it must live as long as the connection, not
	// end with the handshake: a cancelled one silently leaves no GET stream.
	life, end := context.WithCancel(context.Background())
	if err := c.Start(life); err != nil {
		end()
		return nil, fmt.Errorf("start client: %w", err)
	}
	conn := &Conn{c: c, end: end}
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
	if _, err := c.Initialize(ctx, req); err != nil {
		return nil, errors.Join(fmt.Errorf("initialize: %w", err), conn.Close())
	}
	return conn, nil
}

// Tools lists the tools the server offers.
func (c *Conn) Tools(ctx context.Context) ([]mcp.Tool, error) {
	res, err := c.c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	return res.Tools, nil
}

// Call calls tool and returns its text content, its parts joined by
// newlines, and whether the tool reported an error. err is a protocol or
// transport failure: the call got no answer.
func (c *Conn) Call(ctx context.Context, tool string, args map[string]any) (text string, isError bool, err error) {
	req := mcp.CallToolRequest{}
	req.Params.Name = tool
	req.Params.Arguments = args
	res, err := c.c.CallTool(ctx, req)
	if err != nil {
		return "", false, err
	}
	var parts []string
	for _, content := range res.Content {
		if tc, ok := mcp.AsTextContent(content); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n"), res.IsError, nil
}

// Stderr is a stdio server's standard error, when the connection has one.
func (c *Conn) Stderr() (io.Reader, bool) { return client.GetStderr(c.c) }

// Close ends the connection and the client's background work.
func (c *Conn) Close() error {
	err := c.c.Close()
	c.end()
	return err
}
