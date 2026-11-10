package main

import (
	"bytes"
	"context"
	"net/http/httptest"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func echoServer() *server.MCPServer {
	s := server.NewMCPServer("echo", "1", server.WithToolCapabilities(false))
	s.AddTool(mcp.NewTool("echo", mcp.WithString("say")), func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if r.GetString("say", "") == "fail" {
			return mcp.NewToolResultError("asked to fail"), nil
		}
		return mcp.NewToolResultText(r.GetString("say", "")), nil
	})
	return s
}

func TestRun_CallsEachToolAndReportsTheAnswer(t *testing.T) {
	ts := httptest.NewServer(server.NewStreamableHTTPServer(echoServer()))
	t.Cleanup(ts.Close)
	var out bytes.Buffer
	err := run(context.Background(), []string{
		"-url", ts.URL + "/mcp", "-show",
		`[["echo",{"say":"hello"}],["echo",{"say":"fail"}]]`,
	}, &out)
	require.NoError(t, err)
	got := out.String()
	assert.Contains(t, got, `ok  echo {"say":"hello"} 5B`)
	assert.Contains(t, got, "    hello")
	assert.Contains(t, got, `ERR echo {"say":"fail"}`)
	assert.Contains(t, got, "    asked to fail")
}

func TestRun_RefusesAnAmbiguousOrMissingTarget(t *testing.T) {
	var out bytes.Buffer
	assert.ErrorContains(t, run(context.Background(), []string{`[["echo"]]`}, &out), "exactly one of -url or -stdio")
	assert.ErrorContains(t, run(context.Background(), []string{"-url", "x", "-stdio", "y", `[["echo"]]`}, &out), "exactly one of -url or -stdio")
	assert.ErrorContains(t, run(context.Background(), []string{"-url", "x"}, &out), "want the calls")
	assert.ErrorContains(t, run(context.Background(), []string{"-url", "x", "-protocol", "v9", `[["echo"]]`}, &out), "want legacy or modern")
	assert.ErrorContains(t, run(context.Background(), []string{"-url", "x", `[[1,2,3]]`}, &out), "a call is")
}
