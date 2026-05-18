package leyline

import (
	"bytes"
	"errors"
	"io/fs"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"testing"

	graph "github.com/agentic-research/mache/internal/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// stubGraph — minimal graph.Graph implementation for trigger walk tests.
//
// Only the methods TriggerEmbedding actually calls are non-trivial:
// GetNode, ListChildren, ReadContent. The rest of the interface is
// satisfied with safe zero-value stubs.
//
// Each per-method behavior (children, nodes, content, errors) is
// configurable via maps keyed by node ID — this lets a single test
// craft "ListChildren errors on subtree X but works on subtree Y"
// without writing a fresh struct each time.
// ---------------------------------------------------------------------------

type stubGraph struct {
	mu sync.Mutex

	// id → child IDs.
	children map[string][]string
	// id → error to return from ListChildren (overrides children).
	childrenErr map[string]error

	// id → *graph.Node returned by GetNode.
	nodes map[string]*graph.Node

	// id → byte content (returned in full from ReadContent).
	content map[string][]byte
	// id → error to return from ReadContent (overrides content).
	contentErr map[string]error
}

func newStubGraph() *stubGraph {
	return &stubGraph{
		children:    make(map[string][]string),
		childrenErr: make(map[string]error),
		nodes:       make(map[string]*graph.Node),
		content:     make(map[string][]byte),
		contentErr:  make(map[string]error),
	}
}

func (s *stubGraph) addDir(id string, children ...string) {
	s.nodes[id] = &graph.Node{ID: id, Mode: fs.ModeDir}
	s.children[id] = children
}

func (s *stubGraph) addFile(id, body string) {
	s.nodes[id] = &graph.Node{ID: id, Mode: 0}
	s.content[id] = []byte(body)
}

func (s *stubGraph) GetNode(id string) (*graph.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[id]
	if !ok {
		return nil, graph.ErrNotFound
	}
	return n, nil
}

func (s *stubGraph) ListChildren(id string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err, ok := s.childrenErr[id]; ok {
		return nil, err
	}
	return s.children[id], nil
}

func (s *stubGraph) ListChildStats(id string) ([]graph.NodeStat, error) {
	return nil, nil
}

func (s *stubGraph) ReadContent(id string, buf []byte, offset int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err, ok := s.contentErr[id]; ok {
		return 0, err
	}
	data, ok := s.content[id]
	if !ok {
		return 0, nil
	}
	if offset >= int64(len(data)) {
		return 0, nil
	}
	return copy(buf, data[offset:]), nil
}

func (s *stubGraph) GetCallers(token string) ([]*graph.Node, error) { return nil, nil }
func (s *stubGraph) GetCallees(id string) ([]*graph.Node, error)    { return nil, nil }
func (s *stubGraph) Invalidate(id string)                           {}
func (s *stubGraph) Act(id, action, payload string) (*graph.ActionResult, error) {
	return nil, graph.ErrActNotSupported
}

// ---------------------------------------------------------------------------
// captureLogs swaps log.Default's output, returns a getter for the buffer.
// Restores on test cleanup.
// ---------------------------------------------------------------------------

func captureLogs(t *testing.T) func() string {
	t.Helper()
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })
	return func() string { return buf.String() }
}

// pointEmbedAt sets the LEYLINE_SOCKET env var so DiscoverOrStart returns
// the given mock socket path. Returns nothing; t.Setenv handles cleanup.
func pointEmbedAt(t *testing.T, sockPath string) {
	t.Helper()
	t.Setenv("LEYLINE_SOCKET", sockPath)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestTriggerEmbedding_ReadContentError_LogsAndContinues guards the
// silent-corpus-truncation bug: a ReadContent error on one node must
// surface in logs AND must not abort the walk for the next node.
func TestTriggerEmbedding_ReadContentError_LogsAndContinues(t *testing.T) {
	logs := captureLogs(t)

	var embedded []NodeContent
	sockPath := mockServer(t, func(req map[string]any) map[string]any {
		switch req["op"] {
		case "embed_status":
			return map[string]any{"ready": true}
		case "embed_content":
			raw, _ := req["nodes"].([]any)
			for _, r := range raw {
				m, _ := r.(map[string]any)
				id, _ := m["id"].(string)
				content, _ := m["content"].(string)
				embedded = append(embedded, NodeContent{ID: id, Content: content})
			}
			return map[string]any{"embedded": float64(len(raw))}
		}
		return map[string]any{"error": "unexpected op"}
	})
	pointEmbedAt(t, sockPath)

	g := newStubGraph()
	g.addDir("", "root")
	g.addDir("root", "good", "bad")
	g.addFile("good", "good content")
	g.addFile("bad", "(unread)")
	g.contentErr["bad"] = errors.New("synthetic read failure")

	TriggerEmbedding(g, 10)

	out := logs()
	assert.Contains(t, out, "embed trigger: read bad:",
		"ReadContent error must be logged with the offending node id")
	assert.Contains(t, out, "synthetic read failure",
		"underlying error must surface in the log line")

	// Walk must have continued past `bad` and embedded `good`.
	require.Len(t, embedded, 1, "good file must still be pushed despite bad sibling")
	assert.Equal(t, "good", embedded[0].ID)
	assert.Equal(t, "good content", embedded[0].Content)
}

// TestTriggerEmbedding_ListChildrenError_SkipsSubtreeButContinues
// pins the partial-corpus contract: a ListChildren error on one
// subtree must log + skip that subtree, but the outer walk over
// the other roots must still complete.
func TestTriggerEmbedding_ListChildrenError_SkipsSubtreeButContinues(t *testing.T) {
	logs := captureLogs(t)

	var embedded []NodeContent
	sockPath := mockServer(t, func(req map[string]any) map[string]any {
		switch req["op"] {
		case "embed_status":
			return map[string]any{"ready": true}
		case "embed_content":
			raw, _ := req["nodes"].([]any)
			for _, r := range raw {
				m, _ := r.(map[string]any)
				id, _ := m["id"].(string)
				content, _ := m["content"].(string)
				embedded = append(embedded, NodeContent{ID: id, Content: content})
			}
			return map[string]any{"embedded": float64(len(raw))}
		}
		return map[string]any{"error": "unexpected op"}
	})
	pointEmbedAt(t, sockPath)

	g := newStubGraph()
	// Two top-level roots; the broken one errors on ListChildren,
	// the healthy one yields one file we expect to see embedded.
	g.addDir("", "broken", "healthy")
	g.nodes["broken"] = &graph.Node{ID: "broken", Mode: fs.ModeDir}
	g.childrenErr["broken"] = errors.New("synthetic listchildren failure")
	g.addDir("healthy", "file1")
	g.addFile("file1", "alpha")

	TriggerEmbedding(g, 10)

	out := logs()
	assert.Contains(t, out, "embed trigger: list children",
		"ListChildren error must produce a log line")
	assert.Contains(t, out, "broken",
		"failing subtree id must be in the log so operators can trace")
	assert.Contains(t, out, "synthetic listchildren failure",
		"underlying error must surface")

	// Outer walk continued past the broken subtree.
	require.Len(t, embedded, 1, "healthy subtree must still be indexed when sibling subtree errors")
	assert.Equal(t, "file1", embedded[0].ID)
}

// brokenServer accepts a connection on a Unix socket and replies to
// every line with a bare newline (empty JSON body). The client's
// SendOp then trips json.Unmarshal with "unexpected end of JSON
// input", which is exactly what a real transport / parse failure
// in embed_status looks like to TriggerEmbedding.
//
// Inlined here (not added to mockServer) because the existing helper
// always returns a json.Marshal-encoded map — it can't simulate a
// malformed response without changing its signature.
func brokenStatusServer(t *testing.T) string {
	t.Helper()
	// macOS Unix socket paths are capped at 104 bytes; t.TempDir() can
	// blow past that. Use /tmp like mockServer does.
	dir, err := os.MkdirTemp("/tmp", "leyline-broken-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockPath := dir + "/broken.sock"
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
				defer c.Close() //nolint:errcheck
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if err != nil || n == 0 {
						return
					}
					// Reply with bare newline — empty payload causes
					// json.Unmarshal to fail on the client side.
					if _, err := c.Write([]byte("\n")); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return sockPath
}

// TestTriggerEmbedding_StatusError_DistinctLog asserts that a Status
// RPC failure (transport / parse error) logs differently from the
// daemon explicitly reporting `!Ready`. Before the fix both cases
// emitted the same "not enabled" line, making the failure mode
// indistinguishable in the wild.
func TestTriggerEmbedding_StatusError_DistinctLog(t *testing.T) {
	logs := captureLogs(t)

	sockPath := brokenStatusServer(t)
	pointEmbedAt(t, sockPath)

	g := newStubGraph()
	g.addDir("", "root")
	g.addDir("root", "file1")
	g.addFile("file1", "should not reach")

	TriggerEmbedding(g, 10)

	out := logs()
	assert.Contains(t, out, "embed trigger: status check failed",
		"status RPC error must log distinctly from the !Ready branch")
	assert.NotContains(t, out, "embeddings disabled on daemon",
		"status error must NOT collapse into the disabled-daemon log")
}

// TestTriggerEmbedding_StatusNotReady_DistinctLog is the symmetric
// case: the daemon successfully responded, just told us it isn't
// ready. Operator-facing message must say so plainly.
func TestTriggerEmbedding_StatusNotReady_DistinctLog(t *testing.T) {
	logs := captureLogs(t)

	sockPath := mockServer(t, func(req map[string]any) map[string]any {
		if req["op"] == "embed_status" {
			// Successful response, ready=false.
			return map[string]any{"ready": false}
		}
		return map[string]any{}
	})
	pointEmbedAt(t, sockPath)

	g := newStubGraph()
	g.addDir("", "root")

	TriggerEmbedding(g, 10)

	out := logs()
	assert.Contains(t, out, "embed trigger: embeddings disabled on daemon",
		"!Ready must produce the disabled-daemon log line")
	assert.NotContains(t, out, "status check failed",
		"successful !Ready response must NOT be reported as a status check failure")
	// Defensive: make sure these two distinct log strings are never
	// emitted simultaneously for a single status outcome.
	assert.Equal(t, 1, strings.Count(out, "embed trigger:"),
		"exactly one embed-trigger log line expected for a clean !Ready")
}
