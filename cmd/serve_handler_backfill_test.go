package cmd

// Coverage backfill for the per-tool handler files extracted from the
// pre-decomposition serve_handlers.go god-file. The mechanical split
// (1374 LOC -> 182 LOC + ten per-tool files) couldn't move along the
// pre-existing test coverage for these specific edge paths because git's
// rename detection falls below the 50% threshold once a function's
// surrounding context changes file. Each test below exercises a real
// branch — no shape tests, no smoke tests.

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/leyline"
	"github.com/agentic-research/mache/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// find_callers: error + control-mode hint paths
// ---------------------------------------------------------------------------

// errCallersGraph satisfies the Graph interface by embedding a real
// MemoryStore for the methods the handler doesn't exercise, then
// overrides GetCallers to fail. The handler must convert the backend
// error to an MCP error result (not a Go-level error) so MCP clients
// see a structured failure rather than a transport drop.
type errCallersGraph struct {
	graph.Graph
}

func (errCallersGraph) GetCallers(token string) ([]*graph.Node, error) {
	return nil, errors.New("synthetic backend failure")
}

// TestFindCallers_BackendErrorSurfacesAsMCPError pins makeFindCallersHandler's
// error-wrapping contract: when the underlying Graph.GetCallers
// returns an error, the handler wraps it in NewToolResultError rather
// than letting it propagate. Agents polling a misbehaving backend
// should see "get callers: ..." with IsError=true, not a connection
// drop.
func TestFindCallers_BackendErrorSurfacesAsMCPError(t *testing.T) {
	g := errCallersGraph{Graph: graph.NewMemoryStore()}
	handler := makeFindCallersHandler(g)

	result, err := handler(context.Background(), testutil.MakeRequest(map[string]any{"token": "Anything"}))
	require.NoError(t, err, "handler must not return a Go error — the failure must be an MCP error")
	require.NotNil(t, result)
	assert.True(t, result.IsError, "backend error must surface as MCP error")
	assert.Contains(t, testutil.ResultText(t, result), "get callers:")
	assert.Contains(t, testutil.ResultText(t, result), "synthetic backend failure")
}

// TestFindCallers_ControlModeEmptyHasRetryHint pins the control-mode
// empty-result hint: when serveControl is non-empty (mache running
// against a ley-line daemon's arena-backed control block) and
// GetCallers returns no callers, the handler returns the "[] — daemon
// may still be parsing" hint rather than the bare "[]". Agents need
// this signal to know that an empty result is potentially provisional
// during cold-start parse.
func TestFindCallers_ControlModeEmptyHasRetryHint(t *testing.T) {
	prev := serveControl
	serveControl = "/tmp/synthetic-control.ctrl"
	defer func() { serveControl = prev }()

	store := testutil.BuildTestGraph(t)
	handler := makeFindCallersHandler(store)

	result, err := handler(context.Background(), testutil.MakeRequest(map[string]any{"token": "NonExistent"}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	text := testutil.ResultText(t, result)
	assert.Contains(t, text, "daemon may still be parsing", "control-mode empty must surface retry hint")
	assert.Contains(t, text, "[]", "hint must still include the literal empty-array opener")
}

// ---------------------------------------------------------------------------
// get_sheaf_status: dial-failure + status-error paths
// ---------------------------------------------------------------------------

// TestGetSheafStatus_DialFailureReturnsUnavailable pins the dial-failure
// graceful-degradation path: when DiscoverSocket finds a path
// (LEYLINE_SOCKET points at an existing file) but DialSocket fails
// (the file isn't a live UDS listener, or the listener is rejecting),
// the handler must return the structured {available: false, reason:
// ...} response rather than an MCP error. This is the graceful-
// degradation contract documented at the top of
// serve_handler_get_sheaf_status.go.
func TestGetSheafStatus_DialFailureReturnsUnavailable(t *testing.T) {
	leyline.StopManaged()

	// Create a plain regular file at the socket path so os.Stat
	// succeeds (DiscoverSocket returns the path) but net.Dial fails
	// (it isn't a UDS listener). This is the post-SIGKILL stale-file
	// scenario.
	dir := t.TempDir()
	bogusSock := filepath.Join(dir, "not-a-socket")
	require.NoError(t, os.WriteFile(bogusSock, []byte("not a socket"), 0o600))
	t.Setenv("LEYLINE_SOCKET", bogusSock)

	handler := makeGetSheafStatusHandler(nil)
	result, err := handler(context.Background(), testutil.MakeRequest(nil))
	require.NoError(t, err)
	require.False(t, result.IsError, "dial failure must surface as structured unavailable, not MCP error")

	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(testutil.ResultText(t, result)), &out))
	assert.Equal(t, false, out["available"])
	reason, _ := out["reason"].(string)
	assert.Contains(t, reason, "dial", "reason must say why dialing failed")
	assert.Contains(t, reason, bogusSock, "reason must include the offending path")
}

// TestGetSheafStatus_DaemonErrorReturnsUnavailable pins the daemon-error
// graceful-degradation path: when the daemon is reachable but
// sheaf_status itself returns an error payload (sheaf subsystem
// uninitialized, persistence error, etc.), the handler returns
// {available: false, reason: "sheaf_status: ..."}. Same graceful-
// degradation contract as the dial-failure path — agents must not see
// transport-level errors for a routine status poll.
func TestGetSheafStatus_DaemonErrorReturnsUnavailable(t *testing.T) {
	leyline.StopManaged()
	sockPath := startMockSheafServer(t, func(req map[string]any) map[string]any {
		assert.Equal(t, "sheaf_status", req["op"])
		// SheafClient.Status surfaces any "error" field on the
		// response as a Go error — see internal/leyline/sheaf.go.
		// This is the wire-level contract for daemon failures.
		return map[string]any{
			"error": "sheaf not initialized — call sheaf_compute first",
		}
	})
	t.Setenv("LEYLINE_SOCKET", sockPath)

	handler := makeGetSheafStatusHandler(nil)
	result, err := handler(context.Background(), testutil.MakeRequest(nil))
	require.NoError(t, err)
	require.False(t, result.IsError, "daemon-side error must NOT surface as MCP error")

	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(testutil.ResultText(t, result)), &out))
	assert.Equal(t, false, out["available"], "daemon error must mark status unavailable")
	reason, _ := out["reason"].(string)
	assert.Contains(t, reason, "sheaf_status", "unavailable reason must name the daemon op for triage")
	assert.Contains(t, reason, "not initialized",
		"reason must thread through the daemon's actual error text for diagnosability")
}

// ---------------------------------------------------------------------------
// semantic_search: full daemon-reachable + enrichment paths
// ---------------------------------------------------------------------------

// startMockSemanticServer is the semantic-search counterpart to
// startMockSheafServer — it handles a single op (semantic_search) and
// returns a configurable result set. The handler echoes each query's
// requested k back as len(results) capped to len(canned).
func startMockSemanticServer(t *testing.T, canned []map[string]any, returnError string) string {
	t.Helper()
	// NOTE: t.TempDir() returns paths under $TMPDIR which on macOS
	// (/var/folders/...) exceed the 104-byte sun_path limit for UDS
	// sockets. Keep using a short /tmp base — mirrors startMockSheafServer.
	dir, err := os.MkdirTemp("/tmp", "mache-sem-tool-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "t.sock")

	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				dec := json.NewDecoder(c)
				for {
					var req map[string]any
					if err := dec.Decode(&req); err != nil {
						return
					}
					op, _ := req["op"].(string)
					if op != "semantic_search" {
						_, _ = c.Write([]byte(`{"error":"unexpected op"}` + "\n"))
						continue
					}
					var resp map[string]any
					if returnError != "" {
						resp = map[string]any{"error": returnError}
					} else {
						k := 10
						if v, ok := req["k"].(float64); ok && int(v) > 0 {
							k = int(v)
						}
						if k > len(canned) {
							k = len(canned)
						}
						items := make([]any, k)
						for i := 0; i < k; i++ {
							items[i] = canned[i]
						}
						resp = map[string]any{"results": items}
					}
					data, _ := json.Marshal(resp)
					_, _ = c.Write(append(data, '\n'))
				}
			}(conn)
		}
	}()
	return sockPath
}

// isolateLeyline ensures LEYLINE_SOCKET + HOME point at temp dirs so a
// background daemon (or some other test's well-known socket) can't
// satisfy DiscoverOrStart. Callers set LEYLINE_SOCKET to whatever they
// want exercised.
func isolateLeyline(t *testing.T) {
	t.Helper()
	t.Setenv("LEYLINE_SOCKET", "") // clear any ambient daemon path
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MACHE_NO_LEYLINE", "1") // block auto-download + auto-spawn
	leyline.StopManaged()
}

// TestSemanticSearch_DaemonReturnsError pins the daemon-side error
// path in makeSemanticSearchHandler: when the daemon is reachable AND
// DialSocket succeeds but the semantic_search op itself fails (daemon
// doesn't have embeddings indexed yet), the handler returns the "does
// not support embeddings" message. Same agent-visible diagnosability
// as the dial-fail case but distinct branch.
func TestSemanticSearch_DaemonReturnsError(t *testing.T) {
	isolateLeyline(t)
	sockPath := startMockSemanticServer(t, nil, "embeddings not indexed")
	t.Setenv("LEYLINE_SOCKET", sockPath)

	store := testutil.BuildTestGraph(t)
	handler := makeSemanticSearchHandler(store)

	result, err := handler(context.Background(), testutil.MakeRequest(map[string]any{"query": "Helper"}))
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, testutil.ResultText(t, result), "does not support embeddings",
		"daemon-side semantic_search error must surface as the embeddings-not-available message")
}

// TestSemanticSearch_EmptyResults pins the empty-result short-circuit:
// when the daemon responds successfully with zero hits (rare in
// practice but the pattern is "query too obscure to embed"), the
// handler must short-circuit to "[]" rather than spending cycles on
// graph enrichment of an empty slice.
func TestSemanticSearch_EmptyResults(t *testing.T) {
	isolateLeyline(t)
	sockPath := startMockSemanticServer(t, nil, "")
	t.Setenv("LEYLINE_SOCKET", sockPath)

	store := testutil.BuildTestGraph(t)
	handler := makeSemanticSearchHandler(store)

	result, err := handler(context.Background(), testutil.MakeRequest(map[string]any{"query": "Nothing"}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	assert.Equal(t, "[]", testutil.ResultText(t, result), "zero hits must serialize to the bare empty-array literal")
}

// TestSemanticSearch_EnrichesFileResults pins the graph-enrichment
// path (the bulk of the uncovered code): when the daemon returns
// hits, the handler enriches each one with graph metadata — Type=file
// for leaf nodes, Type=directory for dirs, plus a content snippet for
// files. This is the whole point of the handler living in mache
// rather than agents calling the daemon directly: the enrichment is
// what makes the results actionable. Pin it.
func TestSemanticSearch_EnrichesFileResults(t *testing.T) {
	isolateLeyline(t)

	canned := []map[string]any{
		{"id": "pkg/util/helper/source", "distance": 0.12}, // file
		{"id": "pkg/util/helper", "distance": 0.34},        // directory
		{"id": "pkg/does/not/exist", "distance": 0.99},     // missing node — must not error
	}
	sockPath := startMockSemanticServer(t, canned, "")
	t.Setenv("LEYLINE_SOCKET", sockPath)

	store := testutil.BuildTestGraph(t)
	handler := makeSemanticSearchHandler(store)

	result, err := handler(context.Background(), testutil.MakeRequest(map[string]any{"query": "Helper", "k": float64(3)}))
	require.NoError(t, err)
	require.False(t, result.IsError, "valid daemon response must succeed: %s", testutil.ResultText(t, result))

	type enriched struct {
		Path     string  `json:"path"`
		Distance float64 `json:"distance"`
		Type     string  `json:"type,omitempty"`
		Snippet  string  `json:"snippet,omitempty"`
	}
	var out []enriched
	require.NoError(t, json.Unmarshal([]byte(testutil.ResultText(t, result)), &out))
	require.Len(t, out, 3, "all daemon results must appear in the enriched response")

	// File: type=file, snippet present (Helper source is "func Helper() {}")
	assert.Equal(t, "pkg/util/helper/source", out[0].Path)
	assert.Equal(t, "file", out[0].Type, "leaf node must enrich as file")
	assert.Contains(t, out[0].Snippet, "func Helper",
		"file enrichment must include a real content snippet, not a placeholder")

	// Directory: type=directory, no snippet
	assert.Equal(t, "pkg/util/helper", out[1].Path)
	assert.Equal(t, "directory", out[1].Type, "directory node must enrich as directory")
	assert.Empty(t, out[1].Snippet, "directory results must not carry a snippet")

	// Missing node: no type, no snippet — handler must not error
	assert.Equal(t, "pkg/does/not/exist", out[2].Path)
	assert.Empty(t, out[2].Type, "missing node must drop through without a type")
	assert.Empty(t, out[2].Snippet)
}

// TestSemanticSearch_LongFileTruncatesSnippet pins the snippet
// truncation marker: when a file's content exceeds the 200-byte
// snippet window, the handler appends "..." to signal truncation.
// Without this guard, an agent would see a snippet that happened to
// be exactly 200 bytes long and might assume it's the whole file.
func TestSemanticSearch_LongFileTruncatesSnippet(t *testing.T) {
	isolateLeyline(t)

	// Build a graph with a long-content file.
	store := graph.NewMemoryStore()
	longContent := make([]byte, 500)
	for i := range longContent {
		longContent[i] = 'x'
	}
	store.AddRoot(&graph.Node{ID: "big", Mode: 0, Data: longContent})

	canned := []map[string]any{{"id": "big", "distance": 0.01}}
	sockPath := startMockSemanticServer(t, canned, "")
	t.Setenv("LEYLINE_SOCKET", sockPath)

	handler := makeSemanticSearchHandler(store)
	result, err := handler(context.Background(), testutil.MakeRequest(map[string]any{"query": "anything"}))
	require.NoError(t, err)
	require.False(t, result.IsError, "long-file enrichment must succeed: %s", testutil.ResultText(t, result))

	type enriched struct {
		Snippet string `json:"snippet"`
	}
	var out []enriched
	require.NoError(t, json.Unmarshal([]byte(testutil.ResultText(t, result)), &out))
	require.Len(t, out, 1)
	assert.Equal(t, 200+len("..."), len(out[0].Snippet),
		"snippet must be exactly the first 200 bytes plus the truncation marker")
	assert.True(t, len(out[0].Snippet) > 0)
	// Confirm the marker is at the end, not somewhere in the middle.
	assert.Equal(t, "...", out[0].Snippet[len(out[0].Snippet)-3:])
}
