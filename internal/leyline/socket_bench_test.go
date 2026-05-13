package leyline

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// benchDaemon mirrors cmd/uds_graph_test.go::stubDaemon but lives here so
// the SendOp / SendOpInto benches can decode their own responses without
// hitting a circular import on the cmd package.
func benchDaemon(b *testing.B, handler func(map[string]any) map[string]any) string {
	b.Helper()
	dir, err := os.MkdirTemp("/tmp", "uds-bench-*")
	if err != nil {
		b.Fatalf("MkdirTemp: %v", err)
	}
	b.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "t.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		b.Fatalf("Listen: %v", err)
	}
	b.Cleanup(func() { _ = ln.Close() })

	var wg sync.WaitGroup
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
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
					out, _ := json.Marshal(resp)
					out = append(out, '\n')
					if _, err := c.Write(out); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	b.Cleanup(wg.Wait)
	return sockPath
}

// realistic list_children response: 64 entries, each with the same
// field set the post-b0ea2e daemon emits (kind=Int32, size as quoted
// JSON string). Matches the shape udsGraph.ListChildStats decodes.
func benchListChildrenResponse() map[string]any {
	children := make([]any, 64)
	for i := range children {
		children[i] = map[string]any{
			"id":        "/pkg/dir/file_" + itoa(i) + ".go",
			"parent_id": "/pkg/dir",
			"name":      "file_" + itoa(i) + ".go",
			"kind":      0,
			"size":      "4096",
		}
	}
	return map[string]any{"ok": true, "children": children}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// BenchmarkSendOp_MapDecode is the baseline: the pre-PR-#372 path that
// decoded into map[string]any. Reads silently zero Int64 fields on the
// post-b0ea2e wire — these benches use the v0.3.0 shape so the comparison
// is apples-to-apples on serialization cost.
func BenchmarkSendOp_MapDecode(b *testing.B) {
	resp := benchListChildrenResponse()
	sock := benchDaemon(b, func(req map[string]any) map[string]any { return resp })
	c, err := DialSocket(sock)
	if err != nil {
		b.Fatalf("dial: %v", err)
	}
	b.Cleanup(func() { _ = c.Close() })

	req := map[string]any{"op": "list_children", "id": "/pkg/dir"}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := c.SendOp(req); err != nil {
			b.Fatalf("SendOp: %v", err)
		}
	}
}

// BenchmarkSendOpInto_TypedDecode is the new path: typed struct decode
// via SendOpInto into ListChildrenResponse. The PR's correctness win
// shouldn't cost meaningful throughput.
func BenchmarkSendOpInto_TypedDecode(b *testing.B) {
	resp := benchListChildrenResponse()
	sock := benchDaemon(b, func(req map[string]any) map[string]any { return resp })
	c, err := DialSocket(sock)
	if err != nil {
		b.Fatalf("dial: %v", err)
	}
	b.Cleanup(func() { _ = c.Close() })

	req := map[string]any{"op": "list_children", "id": "/pkg/dir"}
	b.ReportAllocs()
	for b.Loop() {
		var dest ListChildrenResponse
		if err := c.SendOpInto(req, &dest); err != nil {
			b.Fatalf("SendOpInto: %v", err)
		}
	}
}

// --- per-op pairs covering the remaining udsGraph surface ---
//
// Each pair benches the same wire payload through both decode paths
// (map vs typed). Pairs let benchstat report apples-to-apples deltas
// per op; aggregating across them captures the full PR #372 surface.

func benchGetNodeResponse() map[string]any {
	return map[string]any{
		"ok": true,
		"node": map[string]any{
			"id":        "/pkg/foo.go",
			"parent_id": "/pkg",
			"name":      "foo.go",
			"kind":      0,
			"size":      "8192",
			"record":    "package foo\n\nfunc Foo() {}\n",
		},
	}
}

func BenchmarkSendOp_GetNode_Map(b *testing.B) {
	resp := benchGetNodeResponse()
	sock := benchDaemon(b, func(req map[string]any) map[string]any { return resp })
	c, err := DialSocket(sock)
	if err != nil {
		b.Fatalf("dial: %v", err)
	}
	b.Cleanup(func() { _ = c.Close() })
	req := map[string]any{"op": "get_node", "id": "/pkg/foo.go"}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := c.SendOp(req); err != nil {
			b.Fatalf("SendOp: %v", err)
		}
	}
}

func BenchmarkSendOpInto_GetNode_Typed(b *testing.B) {
	resp := benchGetNodeResponse()
	sock := benchDaemon(b, func(req map[string]any) map[string]any { return resp })
	c, err := DialSocket(sock)
	if err != nil {
		b.Fatalf("dial: %v", err)
	}
	b.Cleanup(func() { _ = c.Close() })
	req := map[string]any{"op": "get_node", "id": "/pkg/foo.go"}
	b.ReportAllocs()
	for b.Loop() {
		var dest GetNodeResponse
		if err := c.SendOpInto(req, &dest); err != nil {
			b.Fatalf("SendOpInto: %v", err)
		}
	}
}

// 8 KB content payload — realistic mid-sized Go source file.
func benchReadContentResponse() map[string]any {
	body := make([]byte, 8192)
	for i := range body {
		body[i] = byte('a' + (i % 26))
	}
	return map[string]any{"ok": true, "content": string(body)}
}

func BenchmarkSendOp_ReadContent_Map(b *testing.B) {
	resp := benchReadContentResponse()
	sock := benchDaemon(b, func(req map[string]any) map[string]any { return resp })
	c, err := DialSocket(sock)
	if err != nil {
		b.Fatalf("dial: %v", err)
	}
	b.Cleanup(func() { _ = c.Close() })
	req := map[string]any{"op": "read_content", "id": "/pkg/foo.go"}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := c.SendOp(req); err != nil {
			b.Fatalf("SendOp: %v", err)
		}
	}
}

func BenchmarkSendOpInto_ReadContent_Typed(b *testing.B) {
	resp := benchReadContentResponse()
	sock := benchDaemon(b, func(req map[string]any) map[string]any { return resp })
	c, err := DialSocket(sock)
	if err != nil {
		b.Fatalf("dial: %v", err)
	}
	b.Cleanup(func() { _ = c.Close() })
	req := map[string]any{"op": "read_content", "id": "/pkg/foo.go"}
	b.ReportAllocs()
	for b.Loop() {
		var dest ReadContentResponse
		if err := c.SendOpInto(req, &dest); err != nil {
			b.Fatalf("SendOpInto: %v", err)
		}
	}
}

// 32 Ref entries — realistic for a moderately-referenced symbol.
func benchFindCallersResponse() map[string]any {
	callers := make([]any, 32)
	for i := range callers {
		callers[i] = map[string]any{
			"node_id":   "/pkg/caller_" + itoa(i) + "/Use",
			"source_id": "/pkg/caller_" + itoa(i) + "/Use/source",
		}
	}
	return map[string]any{"ok": true, "callers": callers}
}

func BenchmarkSendOp_FindCallers_Map(b *testing.B) {
	resp := benchFindCallersResponse()
	sock := benchDaemon(b, func(req map[string]any) map[string]any { return resp })
	c, err := DialSocket(sock)
	if err != nil {
		b.Fatalf("dial: %v", err)
	}
	b.Cleanup(func() { _ = c.Close() })
	req := map[string]any{"op": "find_callers", "token": "Validate"}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := c.SendOp(req); err != nil {
			b.Fatalf("SendOp: %v", err)
		}
	}
}

func BenchmarkSendOpInto_FindCallers_Typed(b *testing.B) {
	resp := benchFindCallersResponse()
	sock := benchDaemon(b, func(req map[string]any) map[string]any { return resp })
	c, err := DialSocket(sock)
	if err != nil {
		b.Fatalf("dial: %v", err)
	}
	b.Cleanup(func() { _ = c.Close() })
	req := map[string]any{"op": "find_callers", "token": "Validate"}
	b.ReportAllocs()
	for b.Loop() {
		var dest FindCallersResponse
		if err := c.SendOpInto(req, &dest); err != nil {
			b.Fatalf("SendOpInto: %v", err)
		}
	}
}

// 16 callees — typical fan-out for a single Go function.
func benchFindCalleesResponse() map[string]any {
	callees := make([]any, 16)
	for i := range callees {
		callees[i] = map[string]any{
			"node_id":   "/pkg/dep_" + itoa(i) + "/Func",
			"source_id": "/pkg/dep_" + itoa(i) + "/Func/source",
		}
	}
	return map[string]any{"ok": true, "callees": callees}
}

func BenchmarkSendOp_FindCallees_Map(b *testing.B) {
	resp := benchFindCalleesResponse()
	sock := benchDaemon(b, func(req map[string]any) map[string]any { return resp })
	c, err := DialSocket(sock)
	if err != nil {
		b.Fatalf("dial: %v", err)
	}
	b.Cleanup(func() { _ = c.Close() })
	req := map[string]any{"op": "find_callees", "id": "/pkg/foo/Func"}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := c.SendOp(req); err != nil {
			b.Fatalf("SendOp: %v", err)
		}
	}
}

func BenchmarkSendOpInto_FindCallees_Typed(b *testing.B) {
	resp := benchFindCalleesResponse()
	sock := benchDaemon(b, func(req map[string]any) map[string]any { return resp })
	c, err := DialSocket(sock)
	if err != nil {
		b.Fatalf("dial: %v", err)
	}
	b.Cleanup(func() { _ = c.Close() })
	req := map[string]any{"op": "find_callees", "id": "/pkg/foo/Func"}
	b.ReportAllocs()
	for b.Loop() {
		var dest FindCalleesResponse
		if err := c.SendOpInto(req, &dest); err != nil {
			b.Fatalf("SendOpInto: %v", err)
		}
	}
}
