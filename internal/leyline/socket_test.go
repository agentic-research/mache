package leyline

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mockServer starts a UDS server that echoes back canned responses.
// handler receives each parsed JSON request and returns a response map.
// Uses /tmp for socket paths to avoid macOS 104-byte UDS path limit.
func mockServer(t *testing.T, handler func(map[string]any) map[string]any) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "leyline-test-*")
	if err != nil {
		t.Fatalf("mkdirtemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "t.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			go func(c net.Conn) {
				defer c.Close()
				rd := bufio.NewReader(c)
				for {
					line, err := rd.ReadString('\n')
					if err != nil {
						return
					}
					var req map[string]any
					if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &req); err != nil {
						resp, _ := json.Marshal(map[string]any{"error": "bad json"})
						c.Write(append(resp, '\n'))
						continue
					}
					resp := handler(req)
					data, _ := json.Marshal(resp)
					c.Write(append(data, '\n'))
				}
			}(conn)
		}
	}()

	return sockPath
}

func TestDialSocket_ConnectsAndCloses(t *testing.T) {
	sockPath := mockServer(t, func(req map[string]any) map[string]any {
		return map[string]any{"ok": true}
	})

	client, err := DialSocket(sockPath)
	if err != nil {
		t.Fatalf("DialSocket: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Double close should be safe
	if err := client.Close(); err != nil {
		t.Fatalf("double Close: %v", err)
	}
}

func TestSendOp_RoundTrip(t *testing.T) {
	sockPath := mockServer(t, func(req map[string]any) map[string]any {
		op, _ := req["op"].(string)
		return map[string]any{"echo_op": op, "ok": true}
	})

	client, err := DialSocket(sockPath)
	if err != nil {
		t.Fatalf("DialSocket: %v", err)
	}
	defer client.Close()

	resp, err := client.SendOp(map[string]any{"op": "status"})
	if err != nil {
		t.Fatalf("SendOp: %v", err)
	}
	if resp["echo_op"] != "status" {
		t.Errorf("expected echo_op=status, got %v", resp["echo_op"])
	}
}

func TestTool_FormatsCorrectly(t *testing.T) {
	sockPath := mockServer(t, func(req map[string]any) map[string]any {
		name, _ := req["name"].(string)
		args, _ := req["args"].(map[string]any)
		file, _ := args["file"].(string)
		return map[string]any{
			"ok":         true,
			"tool":       name,
			"generation": 42,
			"stats":      map[string]any{"symbols": 15, "hovers": 15, "diagnostics": 3},
			"file_echo":  file,
		}
	})

	client, err := DialSocket(sockPath)
	if err != nil {
		t.Fatalf("DialSocket: %v", err)
	}
	defer client.Close()

	resp, err := client.Tool("lsp", map[string]any{"file": "/tmp/main.go"})
	if err != nil {
		t.Fatalf("Tool: %v", err)
	}
	if resp["tool"] != "lsp" {
		t.Errorf("expected tool=lsp, got %v", resp["tool"])
	}
	if resp["file_echo"] != "/tmp/main.go" {
		t.Errorf("expected file_echo=/tmp/main.go, got %v", resp["file_echo"])
	}
	// generation comes back as float64 from JSON
	if gen, ok := resp["generation"].(float64); !ok || gen != 42 {
		t.Errorf("expected generation=42, got %v", resp["generation"])
	}
}

func TestTool_ReturnsErrorOnToolFailure(t *testing.T) {
	sockPath := mockServer(t, func(req map[string]any) map[string]any {
		return map[string]any{"error": "unknown tool: bad"}
	})

	client, err := DialSocket(sockPath)
	if err != nil {
		t.Fatalf("DialSocket: %v", err)
	}
	defer client.Close()

	_, err = client.Tool("bad", nil)
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
	if !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("expected 'unknown tool' in error, got: %v", err)
	}
}

func TestQuery_ParsesRows(t *testing.T) {
	sockPath := mockServer(t, func(req map[string]any) map[string]any {
		return map[string]any{
			"rows": []any{
				[]any{"node_id_1", "hover text 1"},
				[]any{"node_id_2", "hover text 2"},
			},
		}
	})

	client, err := DialSocket(sockPath)
	if err != nil {
		t.Fatalf("DialSocket: %v", err)
	}
	defer client.Close()

	rows, err := client.Query("SELECT node_id, hover_text FROM _lsp_hover")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if rows[0][0] != "node_id_1" {
		t.Errorf("expected node_id_1, got %v", rows[0][0])
	}
}

func TestDialSocket_ErrorOnMissingSocket(t *testing.T) {
	_, err := DialSocket("/tmp/nonexistent-leyline-test.sock")
	if err == nil {
		t.Fatal("expected error for missing socket")
	}
}

func TestSetDeadline(t *testing.T) {
	sockPath := mockServer(t, func(req map[string]any) map[string]any {
		// Simulate a slow response
		time.Sleep(200 * time.Millisecond)
		return map[string]any{"ok": true}
	})

	client, err := DialSocket(sockPath)
	if err != nil {
		t.Fatalf("DialSocket: %v", err)
	}
	defer client.Close()

	// Set a very short deadline
	client.SetDeadline(time.Now().Add(10 * time.Millisecond))
	_, err = client.SendOp(map[string]any{"op": "status"})
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestDiscoverSocket_EnvVar(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	// Create the file so Stat succeeds
	f, _ := os.Create(sockPath)
	f.Close()

	t.Setenv("LEYLINE_SOCKET", sockPath)
	found, err := DiscoverSocket()
	if err != nil {
		t.Fatalf("DiscoverSocket: %v", err)
	}
	if found != sockPath {
		t.Errorf("expected %s, got %s", sockPath, found)
	}
}

func TestDiscoverSocket_EnvVarMissing(t *testing.T) {
	t.Setenv("LEYLINE_SOCKET", "/tmp/nonexistent-leyline-test.sock")
	_, err := DiscoverSocket()
	if err == nil {
		t.Fatal("expected error for missing socket")
	}
}
