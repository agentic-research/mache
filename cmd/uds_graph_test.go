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
//
// Size values are JSON-string-encoded to match the post-b0ea2e capnp-json
// wire shape (Int64 emitted as quoted strings for JS Number compatibility).
func TestListChildren_ParsesObjectsNotStrings(t *testing.T) {
	sock := stubDaemon(t, func(req map[string]any) map[string]any {
		if req["op"] != "list_children" {
			return map[string]any{"ok": false, "error": "unexpected op"}
		}
		return map[string]any{
			"ok": true,
			"children": []any{
				map[string]any{"id": "/root/a", "name": "a", "kind": 1, "size": "0"},
				map[string]any{"id": "/root/b", "name": "b", "kind": 0, "size": "42"},
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
					map[string]any{"id": "/root/dir", "name": "dir", "kind": 1, "size": "0"},
					map[string]any{"id": "/root/file.go", "name": "file.go", "kind": 0, "size": "128"},
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

// TestGetNode_DirKindNoContentRef pins the kind=dir branch of GetNode:
// directories must NOT carry a ContentRef (they have no content to read).
// Asserts the dir-mode bit is set and Ref stays nil. Counterpart of the
// file-kind coverage in TestGetNode_DecodesSizeAsJSONString.
func TestGetNode_DirKindNoContentRef(t *testing.T) {
	sock := stubDaemon(t, func(req map[string]any) map[string]any {
		return map[string]any{
			"ok": true,
			"node": map[string]any{
				"id":   "/pkg",
				"name": "pkg",
				"kind": 1, // dir
				"size": "0",
			},
		}
	})

	g, err := newUDSGraph(sock)
	require.NoError(t, err)
	defer func() { _ = g.Close() }()

	node, err := g.GetNode("/pkg")
	require.NoError(t, err)
	require.True(t, node.Mode.IsDir(), "kind=1 must set ModeDir")
	require.Nil(t, node.Ref, "directories must not carry a ContentRef")
}

// TestGetNode_RecordPopulatesData pins the `record` → `node.Data` path.
// When the daemon returns a non-empty `record` (the rendered file content
// from SQLite), GetNode must surface it as `node.Data` so callers like
// ReadContent can short-circuit through the in-band bytes.
func TestGetNode_RecordPopulatesData(t *testing.T) {
	const wantRecord = "package main\n\nfunc main() {}\n"
	sock := stubDaemon(t, func(req map[string]any) map[string]any {
		return map[string]any{
			"ok": true,
			"node": map[string]any{
				"id":     "/pkg/main.go",
				"name":   "main.go",
				"kind":   0,
				"size":   "30",
				"record": wantRecord,
			},
		}
	})

	g, err := newUDSGraph(sock)
	require.NoError(t, err)
	defer func() { _ = g.Close() }()

	node, err := g.GetNode("/pkg/main.go")
	require.NoError(t, err)
	require.Equal(t, wantRecord, string(node.Data),
		"non-empty `record` field must populate node.Data verbatim")
}

// TestGetNode_MissingNodeIsNotFound pins the {ok:true, node:null} edge:
// daemon ack but no node payload must surface as graph.ErrNotFound, not
// a successful empty-node return.
func TestGetNode_MissingNodeIsNotFound(t *testing.T) {
	sock := stubDaemon(t, func(req map[string]any) map[string]any {
		return map[string]any{"ok": true} // no `node` key
	})

	g, err := newUDSGraph(sock)
	require.NoError(t, err)
	defer func() { _ = g.Close() }()

	_, err = g.GetNode("/missing")
	require.ErrorIs(t, err, graph.ErrNotFound,
		"ok:true with no node payload must surface as ErrNotFound")
}

// TestReadContent_DecodesContentField pins the typed-decode round-trip
// for the read_content op. The daemon emits `{"ok":true,"content":"..."}`;
// the consumer must slice the returned bytes into the caller's buffer.
// No test existed for this path before — silent regressions in the
// content decode would not have been caught.
func TestReadContent_DecodesContentField(t *testing.T) {
	const wantContent = "hello mache\n"
	sock := stubDaemon(t, func(req map[string]any) map[string]any {
		if req["op"] != "read_content" {
			return map[string]any{"ok": false, "error": "unexpected op"}
		}
		return map[string]any{
			"ok":      true,
			"content": wantContent,
		}
	})

	g, err := newUDSGraph(sock)
	require.NoError(t, err)
	defer func() { _ = g.Close() }()

	buf := make([]byte, 64)
	n, err := g.ReadContent("/file.txt", buf, 0)
	require.NoError(t, err)
	require.Equal(t, len(wantContent), n)
	require.Equal(t, wantContent, string(buf[:n]))
}

// TestReadContent_ErrorEnvelopeIsNotFound pins that {ok:false} on
// read_content surfaces as graph.ErrNotFound, matching the contract
// SQLiteGraph uses for stale node IDs.
func TestReadContent_ErrorEnvelopeIsNotFound(t *testing.T) {
	sock := stubDaemon(t, func(req map[string]any) map[string]any {
		return map[string]any{"ok": false, "error": "node not found"}
	})

	g, err := newUDSGraph(sock)
	require.NoError(t, err)
	defer func() { _ = g.Close() }()

	_, err = g.ReadContent("/stale", make([]byte, 16), 0)
	require.ErrorIs(t, err, graph.ErrNotFound)
}

// TestGetCallers_DecodesNodeIDs pins the typed-decode round-trip for
// find_callers. Each `callers[]` entry's `node_id` must surface as a
// graph.Node. No test existed for this path before — the refactor
// from map[string]any to typed Ref structs was uncovered.
func TestGetCallers_DecodesNodeIDs(t *testing.T) {
	sock := stubDaemon(t, func(req map[string]any) map[string]any {
		if req["op"] != "find_callers" {
			return map[string]any{"ok": false, "error": "unexpected op"}
		}
		require.Equal(t, "Validate", req["token"], "find_callers must pass the token verbatim")
		return map[string]any{
			"ok": true,
			"callers": []any{
				map[string]any{"node_id": "/pkg/a/UseValidate", "source_id": "/pkg/a/UseValidate/source"},
				map[string]any{"node_id": "/pkg/b/CallValidate", "source_id": "/pkg/b/CallValidate/source"},
				// empty node_id must be skipped (defensive)
				map[string]any{"node_id": "", "source_id": "/junk"},
			},
		}
	})

	g, err := newUDSGraph(sock)
	require.NoError(t, err)
	defer func() { _ = g.Close() }()

	callers, err := g.GetCallers("Validate")
	require.NoError(t, err)
	ids := make([]string, 0, len(callers))
	for _, n := range callers {
		ids = append(ids, n.ID)
	}
	require.ElementsMatch(t,
		[]string{"/pkg/a/UseValidate", "/pkg/b/CallValidate"},
		ids,
		"GetCallers must decode node_id strings; empty entries skipped")
}

// TestListChildren_ErrorNotFoundMapping pins the daemon-error → typed-
// error contract: an {"ok":false,"error":"... not found ..."} envelope
// must surface as graph.ErrNotFound (case-insensitive substring match),
// matching the SQLiteGraph contract for stale directory IDs.
func TestListChildren_ErrorNotFoundMapping(t *testing.T) {
	sock := stubDaemon(t, func(req map[string]any) map[string]any {
		return map[string]any{"ok": false, "error": "Node Not Found in arena"}
	})

	g, err := newUDSGraph(sock)
	require.NoError(t, err)
	defer func() { _ = g.Close() }()

	_, err = g.ListChildren("/stale")
	require.ErrorIs(t, err, graph.ErrNotFound,
		"daemon error containing 'not found' must map to graph.ErrNotFound")
}

// TestListChildren_GenericErrorPropagates pins the non-not-found error
// path: any other error string from the daemon must propagate as a
// generic error (with the daemon's message preserved for debugging).
// Without this, mache would conflate all daemon errors with ErrNotFound.
func TestListChildren_GenericErrorPropagates(t *testing.T) {
	sock := stubDaemon(t, func(req map[string]any) map[string]any {
		return map[string]any{"ok": false, "error": "arena read failed: io/timeout"}
	})

	g, err := newUDSGraph(sock)
	require.NoError(t, err)
	defer func() { _ = g.Close() }()

	_, err = g.ListChildren("/x")
	require.Error(t, err)
	require.NotErrorIs(t, err, graph.ErrNotFound, "non-not-found errors must not collapse to ErrNotFound")
	require.Contains(t, err.Error(), "arena read failed: io/timeout",
		"daemon error message must be preserved in the wrapped error")
}

// TestGetNode_DecodesSizeAsJSONString pins the bug this PR targets:
// post-b0ea2e the daemon emits Int64 fields as quoted JSON strings (e.g.
// `"size":"4096"`). The pre-refactor `map[string]any` + `.(float64)`
// pattern silently zeroed every Int64 field on that wire. Typed decoding
// with `,string` tags must round-trip the value correctly.
//
// Counterpart of TestListChildren_ParsesObjectsNotStrings — that one
// covers the list_children path; this one covers get_node so any future
// regression in the typed decoder is caught for both shapes the daemon
// actually emits Node records in.
func TestGetNode_DecodesSizeAsJSONString(t *testing.T) {
	const wantSize int64 = 4096
	sock := stubDaemon(t, func(req map[string]any) map[string]any {
		if req["op"] != "get_node" {
			return map[string]any{"ok": false, "error": "unexpected op"}
		}
		return map[string]any{
			"ok": true,
			"node": map[string]any{
				"id":        "/pkg/foo.go",
				"parent_id": "/pkg",
				"name":      "foo.go",
				"kind":      0, // file
				"size":      "4096",
			},
		}
	})

	g, err := newUDSGraph(sock)
	require.NoError(t, err)
	defer func() { _ = g.Close() }()

	node, err := g.GetNode("/pkg/foo.go")
	require.NoError(t, err)
	require.NotNil(t, node.Ref, "file node must carry a ContentRef")
	require.Equal(t, wantSize, node.Ref.ContentLen,
		"Size must round-trip through the `,string` json tag")
}

// TestGetNode_RejectsBareNumericSize is the inverse guard: under the
// post-b0ea2e wire contract sizes are quoted strings; if a (broken or
// pre-v0.3.0) daemon emits a bare number, we want a loud error rather
// than a silent zero. This pins the "strict decode is the failure mode"
// design choice flagged on PR review.
func TestGetNode_RejectsBareNumericSize(t *testing.T) {
	sock := stubDaemon(t, func(req map[string]any) map[string]any {
		return map[string]any{
			"ok": true,
			"node": map[string]any{
				"id":   "/pkg/foo.go",
				"kind": 0,
				"size": 4096, // bare number — pre-b0ea2e shape
			},
		}
	})

	g, err := newUDSGraph(sock)
	require.NoError(t, err)
	defer func() { _ = g.Close() }()

	_, err = g.GetNode("/pkg/foo.go")
	require.Error(t, err, "bare numeric size must fail decode, not silently zero")
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
