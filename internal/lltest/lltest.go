// Package lltest provides test doubles for the leyline daemon wire: an
// in-process fake speaking the line-delimited JSON UDS protocol, and a gated
// spawner for the SHA-pinned real binary. Tests point mache at either via
// t.Setenv("LEYLINE_SOCKET", sock) — leyline.DiscoverOrStart honors that env
// var before any spawn/download logic runs.
package lltest

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// Handler produces the response object for one decoded request. The returned
// value is JSON-marshalled and written back as a single line.
type Handler func(req map[string]any) any

// FakeDaemon starts a Unix-domain-socket server that speaks the daemon's
// line-delimited JSON protocol, answering every request via handler. It
// returns the socket path; callers wire it up with
// t.Setenv("LEYLINE_SOCKET", sock). The server tolerates probe connections
// that close without sending (DiscoverOrStart's liveness check does exactly
// that). Shutdown is registered via t.Cleanup.
func FakeDaemon(t *testing.T, handler Handler) string {
	t.Helper()

	// os.MkdirTemp("", ...) uses $TMPDIR which stays under the ~104-byte
	// sun_path limit on macOS, unlike t.TempDir()'s deeply nested paths.
	dir, err := os.MkdirTemp("", "llfake")
	if err != nil {
		t.Fatalf("lltest: create fake daemon dir: %v", err)
	}
	sock := filepath.Join(dir, "d.sock")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatalf("lltest: listen on %s: %v", sock, err)
	}
	t.Cleanup(func() {
		_ = ln.Close()
		_ = os.RemoveAll(dir)
	})

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			go serveConn(conn, handler)
		}
	}()

	return sock
}

func serveConn(conn net.Conn, handler Handler) {
	defer func() { _ = conn.Close() }()
	rd := bufio.NewReader(conn)
	for {
		line, err := rd.ReadBytes('\n')
		if err != nil {
			return // EOF: client closed (liveness probes do this immediately)
		}
		var req map[string]any
		if err := json.Unmarshal(line, &req); err != nil {
			return
		}
		resp, err := json.Marshal(handler(req))
		if err != nil {
			return
		}
		resp = append(resp, '\n')
		if _, err := conn.Write(resp); err != nil {
			return
		}
	}
}
