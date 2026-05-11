package cmd

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/stretchr/testify/require"
)

// stubDaemon spins up a UDS server in /tmp that handles one connection,
// dispatching each line-JSON request to handler. Mirrors the daemon's
// line-protocol (rs/ll-open/cli-lib/src/daemon/socket.rs). Returns the
// socket path.
func stubDaemon(t *testing.T, handler func(map[string]any) map[string]any) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "uds-graph-test-*")
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
				rd := bufio.NewReader(c)
				for {
					line, err := rd.ReadBytes('\n')
					if err != nil {
						return
					}
					var req map[string]any
					if err := json.Unmarshal(line, &req); err != nil {
						return
					}
					resp := handler(req)
					b, _ := json.Marshal(resp)
					b = append(b, '\n')
					if _, err := c.Write(b); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return sockPath
}

// TestListChildren_ParsesObjectsNotStrings is a regression guard for the
// bug where udsGraph treated each `children` entry as a string ID rather
// than the `{id, name, kind, size}` object the LLO daemon actually
// returns (rs/ll-open/cli-lib/src/daemon/ops.rs::op_list_children). The
// old code did `c.(string)` on each entry, so every non-empty directory
// listing came back as an empty slice.
func TestListChildren_ParsesObjectsNotStrings(t *testing.T) {
	sock := stubDaemon(t, func(req map[string]any) map[string]any {
		if req["op"] != "list_children" {
			return map[string]any{"ok": false, "error": "unexpected op"}
		}
		return map[string]any{
			"ok": true,
			"children": []any{
				map[string]any{"id": "/root/a", "name": "a", "kind": 1, "size": 0},
				map[string]any{"id": "/root/b", "name": "b", "kind": 0, "size": 42},
			},
		}
	})

	g, err := newUDSGraph(sock)
	require.NoError(t, err)
	defer func() { _ = g.Close() }()

	ids, err := g.ListChildren("/root")
	require.NoError(t, err)
	require.Equal(t, []string{"/root/a", "/root/b"}, ids, "ListChildren must extract id strings from {id,name,kind,size} objects")
}

func TestListChildStats_SingleShotFromListChildrenResponse(t *testing.T) {
	// Daemon already returns kind + size per child — ListChildStats must
	// build NodeStats directly from that payload rather than calling
	// GetNode per entry (the previous N+1 pattern). This test asserts
	// no get_node ops fire during a ListChildStats call.
	var sawGetNode atomic.Bool
	sock := stubDaemon(t, func(req map[string]any) map[string]any {
		switch req["op"] {
		case "list_children":
			return map[string]any{
				"ok": true,
				"children": []any{
					map[string]any{"id": "/root/dir", "name": "dir", "kind": 1, "size": 0},
					map[string]any{"id": "/root/file.go", "name": "file.go", "kind": 0, "size": 128},
				},
			}
		case "get_node":
			sawGetNode.Store(true)
			return map[string]any{"ok": false}
		}
		return map[string]any{"ok": false}
	})

	g, err := newUDSGraph(sock)
	require.NoError(t, err)
	defer func() { _ = g.Close() }()

	stats, err := g.ListChildStats("/root")
	require.NoError(t, err)
	require.Len(t, stats, 2)
	require.False(t, sawGetNode.Load(), "ListChildStats must not fan out to GetNode per child — kind+size are in the list_children response")

	// Index by ID so the assertions don't depend on map iteration order.
	byID := map[string]graph.NodeStat{}
	for _, s := range stats {
		byID[s.ID] = s
	}
	require.True(t, byID["/root/dir"].IsDir)
	require.EqualValues(t, 0, byID["/root/dir"].ContentSize)
	require.False(t, byID["/root/file.go"].IsDir)
	require.EqualValues(t, 128, byID["/root/file.go"].ContentSize)
}

func TestGetCallees_ResolvesViaDaemonFindCalleesOp(t *testing.T) {
	// LLO 0.2.2 added the `find_callees` daemon op. Mache's udsGraph
	// consumes it directly — no client-side tree-sitter extraction
	// (unlike SQLiteGraph). This test pins the wire shape and
	// asserts dedup + self-edge skipping.
	var calleesCalls atomic.Int32
	sock := stubDaemon(t, func(req map[string]any) map[string]any {
		switch req["op"] {
		case "find_callees":
			calleesCalls.Add(1)
			// daemon op_find_callees returns DISTINCT (node_id, source_id)
			// pairs joined from node_refs ⋈ node_defs.
			return map[string]any{
				"ok": true,
				"callees": []any{
					map[string]any{"node_id": "/pkg/auth/Validate", "source_id": "/pkg/auth/Validate/source"},
					map[string]any{"node_id": "/pkg/auth/Hash", "source_id": "/pkg/auth/Hash/source"},
					// dup of Validate — must be filtered out client-side
					// even though daemon also de-dups; defense in depth.
					map[string]any{"node_id": "/pkg/auth/Validate", "source_id": "/pkg/auth/Validate/source"},
					// self-edge — must be skipped (consumers don't want
					// `find_callees(F)` to list F itself).
					map[string]any{"node_id": "/pkg/auth/Login", "source_id": "/pkg/auth/Login/source"},
				},
			}
		}
		return map[string]any{"ok": false}
	})

	g, err := newUDSGraph(sock)
	require.NoError(t, err)
	defer func() { _ = g.Close() }()

	callees, err := g.GetCallees("/pkg/auth/Login")
	require.NoError(t, err)
	require.EqualValues(t, 1, calleesCalls.Load(), "GetCallees should round-trip exactly one find_callees op")

	ids := make([]string, 0, len(callees))
	for _, n := range callees {
		ids = append(ids, n.ID)
	}
	require.ElementsMatch(t, []string{"/pkg/auth/Validate", "/pkg/auth/Hash"}, ids,
		"self-edge to /pkg/auth/Login skipped; duplicate Validate deduped")
}

func TestGetCallees_EmptyResultIsNotAnError(t *testing.T) {
	// Daemon returning {ok: false} (e.g. unknown node, no refs) should
	// not propagate as an error — other Graph backends treat empty
	// callees as a normal case, not a failure.
	sock := stubDaemon(t, func(req map[string]any) map[string]any {
		return map[string]any{"ok": false, "error": "node not found"}
	})

	g, err := newUDSGraph(sock)
	require.NoError(t, err)
	defer func() { _ = g.Close() }()

	callees, err := g.GetCallees("/unknown")
	require.NoError(t, err)
	require.Empty(t, callees)
}
