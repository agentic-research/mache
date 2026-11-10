package main

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func echoURL(t *testing.T) string {
	ts := httptest.NewServer(server.NewStreamableHTTPServer(echoServer()))
	t.Cleanup(ts.Close)
	return ts.URL + "/mcp"
}

func TestExecute_CallsEachToolAndReportsTheAnswer(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, execute(context.Background(), []string{
		"-url", echoURL(t), "-show",
		`[{"tool":"echo","args":{"say":"hello"}},{"tool":"echo","args":{"say":"fail"}}]`,
	}, &out))
	got := out.String()
	assert.Contains(t, got, `ok  echo {"say":"hello"} 5B`)
	assert.Contains(t, got, "    hello")
	assert.Contains(t, got, `ERR echo {"say":"fail"}`)
	assert.Contains(t, got, "    asked to fail")
}

// A release-parity plan file runs as it is.
func TestExecute_RunsAPlanFile(t *testing.T) {
	plan := filepath.Join(t.TempDir(), "plan.json")
	require.NoError(t, os.WriteFile(plan, []byte(`[{"tool":"echo","args":{"say":"from-file"}}]`), 0o644))
	var out bytes.Buffer
	require.NoError(t, execute(context.Background(), []string{"-url", echoURL(t), "-plan", plan}, &out))
	assert.Contains(t, out.String(), `ok  echo {"say":"from-file"} 9B`)
}

func TestParseArgs_RefusesWhatItCannotRun(t *testing.T) {
	for want, args := range map[string][]string{
		"exactly one of -url or -stdio": {`[{"tool":"echo"}]`},
		"want one plan":                 {"-url", "x"},
		"want legacy or modern":         {"-url", "x", "-protocol", "v9", `[{"tool":"echo"}]`},
		"plan is empty":                 {"-url", "x", `[]`},
	} {
		_, err := parseArgs(args)
		assert.ErrorContains(t, err, want, "%v", args)
	}
	_, err := parseArgs([]string{"-url", "x", "-stdio", "y", `[{"tool":"echo"}]`})
	assert.ErrorContains(t, err, "exactly one of -url or -stdio")
}

// Arguments after -- go to the stdio server, not to the probe's flags.
func TestParseArgs_PassesStdioArgumentsThrough(t *testing.T) {
	o, err := parseArgs([]string{"-stdio", "bin/mache", `[{"tool":"echo"}]`, "--", "serve", "--stdio", "."})
	require.NoError(t, err)
	assert.Equal(t, []string{"serve", "--stdio", "."}, o.stdioArgs)
	assert.Equal(t, "bin/mache", o.stdio)
}
