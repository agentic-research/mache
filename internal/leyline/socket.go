// Package leyline provides Go bindings to the ley-line data plane.
//
// socket.go implements a pure-Go UDS socket client for the ley-line
// control socket (line-delimited JSON). No CGo or build tags required.

package leyline

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SocketClient communicates with a running ley-line daemon over its
// Unix domain control socket ({ctrl}.sock).
type SocketClient struct {
	conn net.Conn
	rd   *bufio.Reader
}

// DialSocket connects to the ley-line control socket at sockPath.
// The socket path is typically derived from the control path:
// e.g. /tmp/leyline.ctrl → /tmp/leyline.sock
func DialSocket(sockPath string) (*SocketClient, error) {
	conn, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", sockPath, err)
	}
	return &SocketClient{
		conn: conn,
		rd:   bufio.NewReader(conn),
	}, nil
}

// DiscoverSocket finds the ley-line socket path by checking:
//  1. LEYLINE_SOCKET environment variable
//  2. ~/.mache/default.sock (well-known kiln deployment path)
//
// Returns the path and nil error if a socket file exists, or an error if
// no socket can be found.
func DiscoverSocket() (string, error) {
	if env := os.Getenv("LEYLINE_SOCKET"); env != "" {
		if _, err := os.Stat(env); err == nil {
			return env, nil
		}
		return "", fmt.Errorf("LEYLINE_SOCKET=%s: %w", env, os.ErrNotExist)
	}

	home, err := os.UserHomeDir()
	if err == nil {
		wellKnown := filepath.Join(home, ".mache", "default.sock")
		if _, err := os.Stat(wellKnown); err == nil {
			return wellKnown, nil
		}
	}

	return "", fmt.Errorf("no ley-line socket found (set LEYLINE_SOCKET or start leyline daemon)")
}

// Close closes the underlying connection. Safe to call multiple times.
func (c *SocketClient) Close() error {
	if c.conn != nil {
		err := c.conn.Close()
		c.conn = nil
		return err
	}
	return nil
}

// SendOp sends a JSON request and reads the JSON response.
// Both are line-delimited (newline-terminated JSON).
func (c *SocketClient) SendOp(req map[string]any) (map[string]any, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	data = append(data, '\n')

	if _, err := c.conn.Write(data); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	line, err := c.rd.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var resp map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &resp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	return resp, nil
}

// Tool invokes a named tool with the given args via the `tool` op.
// Returns the full response map on success.
func (c *SocketClient) Tool(name string, args map[string]any) (map[string]any, error) {
	req := map[string]any{
		"op":   "tool",
		"name": name,
		"args": args,
	}
	resp, err := c.SendOp(req)
	if err != nil {
		return nil, err
	}
	if errMsg, ok := resp["error"]; ok {
		return nil, fmt.Errorf("tool %s: %v", name, errMsg)
	}
	return resp, nil
}

// Query runs a SQL query against the active arena buffer via the `query` op.
// Returns the rows as [][]any.
func (c *SocketClient) Query(sql string) ([][]any, error) {
	resp, err := c.SendOp(map[string]any{
		"op":  "query",
		"sql": sql,
	})
	if err != nil {
		return nil, err
	}
	if errMsg, ok := resp["error"]; ok {
		return nil, fmt.Errorf("query: %v", errMsg)
	}
	rows, ok := resp["rows"]
	if !ok {
		return nil, nil
	}
	// rows is []any where each element is []any
	rawRows, ok := rows.([]any)
	if !ok {
		return nil, fmt.Errorf("unexpected rows type: %T", rows)
	}
	result := make([][]any, len(rawRows))
	for i, r := range rawRows {
		row, ok := r.([]any)
		if !ok {
			continue
		}
		result[i] = row
	}
	return result, nil
}

// SetDeadline sets the read/write deadline on the underlying connection.
func (c *SocketClient) SetDeadline(t time.Time) error {
	return c.conn.SetDeadline(t)
}
