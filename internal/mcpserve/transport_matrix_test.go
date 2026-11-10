package mcpserve

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/lltest"
	"github.com/agentic-research/mache/internal/mcpharness"
	"github.com/agentic-research/mache/internal/projcfg"
)

// matrixTree writes a small source tree for the matrix to serve.
func matrixTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for p, body := range map[string]string{
		"main.go":       "package main\n\nfunc main() { helper() }\n",
		"helper.go":     "package main\n\nfunc helper() {}\n",
		"pkg/a.go":      "package pkg\n\nfunc A() {}\n",
		"pkg/a_test.go": "package pkg\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) { A() }\n",
	} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, filepath.Dir(p)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(root, p), []byte(body), 0o644))
	}
	return root
}

// serveServer is a serve-mode MCP server over a fresh registry, closed when
// the test ends as `mache serve` closes its own on shutdown. An unclosed
// registry is collected while its graphs' SQLite connections are still
// open, and the parse-cache lock goes with it, so the next owner of that
// entry finds it locked by a graph nobody holds.
func serveServer(t *testing.T) *server.MCPServer {
	t.Helper()
	r := newGraphRegistry("", nil)
	t.Cleanup(r.Close)
	return newMCPServer(r)
}

// overviewFiles is get_overview's total_files, failing the test if the call
// did not succeed.
func overviewFiles(t *testing.T, s *mcpharness.Session) int {
	t.Helper()
	var ov struct {
		TotalFiles int `json:"total_files"`
	}
	require.NoError(t, json.Unmarshal([]byte(s.Text("get_overview", nil)), &ov))
	return ov.TotalFiles
}

// The serve-mode server answers the same question the same way however a
// client reaches it: in process or over Streamable HTTP, resolving its
// workspace from roots/list or from a ?project= token. Each cell is a fresh
// server, driven by mcp-go's own client through mcpharness rather than a
// hand-rolled one, which is how a client that holds no GET stream was found
// to get no workspace at all (mache-ab00c9).
func TestTransportMatrix_SameAnswerAcrossTransportsAndClients(t *testing.T) {
	lltest.ResolveBinaryOrSkip(t)
	mcpharness.IsolatedHome(t)
	root := matrixTree(t)
	fresh := func(t *testing.T) http.Handler { return newHTTPServer(serveServer(t)).Handler }

	token, err := projcfg.RegisterProject(root)
	require.NoError(t, err)

	cells := map[string]func(t *testing.T) *mcpharness.Session{
		"in-process, roots": func(t *testing.T) *mcpharness.Session {
			return mcpharness.InProcess(t, serveServer(t), mcpharness.Behavior{Roots: []string{root}})
		},
		"http, roots, GET stream held": func(t *testing.T) *mcpharness.Session {
			return mcpharness.Served(t, fresh(t), "/mcp", mcpharness.Behavior{Roots: []string{root}, Listen: true})
		},
		"http, ?project= token": func(t *testing.T) *mcpharness.Session {
			return mcpharness.Served(t, fresh(t), "/mcp", mcpharness.Behavior{Project: token})
		},
		"http, ?project= token, protocol 2026-07-28": func(t *testing.T) *mcpharness.Session {
			return mcpharness.Served(t, fresh(t), "/mcp", mcpharness.Behavior{Project: token, Protocol: mcpharness.Modern})
		},
	}
	want := 4
	for name, open := range cells {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want, overviewFiles(t, open(t)), "total_files")
		})
	}
}

// A client with no ?project= token and no way to answer roots/list gets
// mache's guidance, not a hang or a graph of the server's working directory.
// On protocol 2026-07-28 that is every client offering roots too: the
// revision removed server-initiated requests, so roots/list cannot be sent.
func TestTransportMatrix_NoWorkspaceIsRefusedWithGuidance(t *testing.T) {
	mcpharness.IsolatedHome(t)
	root := matrixTree(t)
	for name, b := range map[string]mcpharness.Behavior{
		"no roots, no token":                 {},
		"roots offered, protocol 2026-07-28": {Roots: []string{root}, Protocol: mcpharness.Modern},
	} {
		t.Run(name, func(t *testing.T) {
			s := mcpharness.Served(t, newHTTPServer(serveServer(t)).Handler, "/mcp", b)
			text, isError := s.Call("get_overview", nil)
			require.True(t, isError, "served a workspace it was never given: %s", text)
			assert.Contains(t, text, "mache init")
		})
	}
}

// KNOWN GAP, mache-ab00c9: over HTTP, a client that answers roots/list but
// holds no standalone GET stream never receives mache's roots/list, so it gets
// no workspace. mcp-go 1.5.0 sends a server request made during a tool call on
// that call's POST stream for elicitation, but queues ListRoots for the GET
// stream only; the fix belongs there. This pins today's failure, so the day it
// is fixed this test fails and is turned into a cell of the matrix above.
func TestTransportMatrix_RootsWithoutAGETStreamGetNoWorkspace_mache_ab00c9(t *testing.T) {
	lltest.ResolveBinaryOrSkip(t)
	mcpharness.IsolatedHome(t)
	root := matrixTree(t)
	s := mcpharness.Served(t, newHTTPServer(serveServer(t)).Handler, "/mcp", mcpharness.Behavior{Roots: []string{root}})
	text, isError := s.Call("get_overview", nil)
	require.True(t, isError, "roots without a GET stream now resolve: mache-ab00c9 is fixed upstream; move this case into the matrix")
	assert.Contains(t, text, "workspace root unavailable")
}

// `mache serve --mount` serves the composite of its mounts to every session,
// whatever the client does about roots. Every session used to fail: a client
// without roots got no workspace, and one with roots had its root turned
// into a positional source that --mount refuses (mache-e0605e).
func TestTransportMatrix_MountModeServesTheCompositeToEveryClient(t *testing.T) {
	lltest.ResolveBinaryOrSkip(t)
	mcpharness.IsolatedHome(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dirA, "a.go"), []byte("package a\n\nfunc A() { B() }\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dirB, "b.go"), []byte("package b\n\nfunc B() {}\n"), 0o644))
	withServeMounts(t, []string{"alpha=" + dirA, "beta=" + dirB})

	for name, b := range map[string]mcpharness.Behavior{
		"client without roots":   {},
		"client answering roots": {Roots: []string{dirA}},
	} {
		t.Run(name, func(t *testing.T) {
			s := mcpharness.Served(t, newHTTPServer(serveServer(t)).Handler, "/mcp", b)
			listing := s.Text("list_directory", map[string]any{"path": ""})
			assert.Contains(t, listing, `"alpha"`)
			assert.Contains(t, listing, `"beta"`)
			callers := s.Text("find_callers", map[string]any{"token": "B"})
			assert.Contains(t, callers, "alpha/", "find_callers did not cross into the calling mount: %s", callers)
		})
	}
}
