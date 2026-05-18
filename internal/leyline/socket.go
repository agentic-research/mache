// Package leyline provides Go bindings to the ley-line data plane.
//
// socket.go implements a pure-Go UDS socket client for the ley-line
// control socket (line-delimited JSON). No CGo or build tags required.
//
// Auto-spawn: when no running daemon is found but the leyline binary is
// on PATH, DiscoverOrStart transparently launches a daemon subprocess.
// The subprocess is cleaned up when the mache process exits.

package leyline

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// SocketClient communicates with a running ley-line daemon over its
// Unix domain control socket ({ctrl}.sock).
//
// SendOp / SendOpInto are safe for concurrent use: sendMu serializes
// the write-then-read pair so the line-delimited JSON protocol can't
// get crossed when multiple callers (e.g. the file watcher firing
// multiple debounce timers after a save burst) hit the same client
// simultaneously. The protocol is request/response — interleaving
// produces a response for the wrong caller, which the daemon can't
// detect since there's no per-message correlation ID.
//
// Subscribe is the exception: once a Subscribe goroutine takes
// ownership of the read side, ad-hoc SendOp on the same connection
// is unsupported and will race the subscription's event reads. The
// canonical usage is one SocketClient per role (one for ops, one
// for subscribe).
type SocketClient struct {
	conn   net.Conn
	rd     *bufio.Reader
	sendMu sync.Mutex
}

// managed holds state for a leyline daemon subprocess auto-spawned by mache.
// At most one managed daemon per process. Cleaned up on process exit.
var managed struct {
	mu   sync.Mutex
	proc *os.Process
	sock string
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
// no socket can be found. Does NOT auto-start a daemon.
func DiscoverSocket() (string, error) {
	if sock, err := findExistingSocket(); err == nil {
		return sock, nil
	}
	return "", fmt.Errorf("no ley-line socket found (set LEYLINE_SOCKET or start leyline daemon)")
}

// DiscoverOrStart finds a running ley-line daemon socket, or auto-starts
// a managed daemon subprocess if the leyline binary is on PATH.
//
// The managed daemon uses ~/.mache/ as its data directory:
//
//	~/.mache/default.arena  — arena file
//	~/.mache/default.ctrl   — control block
//	~/.mache/default.sock   — UDS socket (what we connect to)
//	~/.mache/mount/         — FUSE/NFS mount point
//
// The subprocess is killed when the mache process exits (via atexit cleanup
// registered on first spawn). Only one managed daemon per process.
func DiscoverOrStart() (string, error) {
	// Fast path: socket already exists (running daemon or env var)
	if sock, err := findExistingSocket(); err == nil {
		return sock, nil
	}

	// Check for previously spawned managed daemon
	managed.mu.Lock()
	defer managed.mu.Unlock()
	if managed.sock != "" {
		if _, err := os.Stat(managed.sock); err == nil {
			return managed.sock, nil
		}
		// Stale — previous daemon died, clear state and try again
		managed.proc = nil
		managed.sock = ""
	}

	// Find the leyline binary: PATH → ~/.mache/bin/ → auto-download
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}

	leylineBin, err := exec.LookPath("leyline")
	if err != nil {
		// Fallback: check ~/.mache/bin/leyline
		localBin := filepath.Join(home, ".mache", "bin", "leyline")
		if _, statErr := os.Stat(localBin); statErr == nil {
			leylineBin = localBin
		} else if os.Getenv("MACHE_NO_LEYLINE") != "" {
			// CI / bundle deployments set this to opt out of the
			// network-fetch path entirely. Surface a clear error
			// rather than attempting a download that's likely to
			// 404 against an unpublished ley-line-open release.
			return "", fmt.Errorf("leyline not on PATH and MACHE_NO_LEYLINE is set; install leyline or unset MACHE_NO_LEYLINE")
		} else {
			// Auto-download from GitHub releases
			downloaded, dlErr := downloadLeyline(localBin)
			if dlErr != nil {
				return "", fmt.Errorf("leyline not on PATH and auto-download failed: %w", dlErr)
			}
			leylineBin = downloaded
		}
	}
	dataDir := filepath.Join(home, ".mache")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", dataDir, err)
	}
	mountDir := filepath.Join(dataDir, "mount")
	if err := os.MkdirAll(mountDir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", mountDir, err)
	}

	arenaPath := filepath.Join(dataDir, "default.arena")
	ctrlPath := filepath.Join(dataDir, "default.ctrl")
	sockPath := filepath.Join(dataDir, "default.sock")

	// Start leyline daemon as a background subprocess.
	// `daemon` creates the UDS socket at <ctrl>.sock that mache connects to.
	// `serve` mounts only (no socket) — wrong for our use case.
	cmd := exec.Command(leylineBin, "daemon",
		"--arena", arenaPath,
		"--arena-size-mib", "64",
		"--control", ctrlPath,
		"--mount", mountDir,
	)
	// Detach from our stdio so it doesn't interfere with MCP transport
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil

	log.Printf("auto-starting leyline daemon: %s", strings.Join(cmd.Args, " "))
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start leyline: %w", err)
	}

	managed.proc = cmd.Process
	managed.sock = sockPath

	// Background goroutine to wait on the process (prevent zombie)
	go func() { _ = cmd.Wait() }()

	// Poll for socket to appear (daemon needs a moment to bind)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			log.Printf("leyline daemon ready (pid=%d, socket=%s)", cmd.Process.Pid, sockPath)
			return sockPath, nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Timed out — kill the process and report
	_ = cmd.Process.Kill()
	managed.proc = nil
	managed.sock = ""
	return "", fmt.Errorf("leyline daemon started but socket %s did not appear within 5s", sockPath)
}

// StopManaged gracefully stops the auto-spawned leyline daemon, if any.
// Sends SIGTERM first so leyline can unmount NFS cleanly, then waits up to
// 3 seconds before falling back to SIGKILL. Without this, macOS shows a
// "Server connections interrupted" dialog for the stale NFS mount.
// Safe to call multiple times. Called automatically by cleanup hooks.
func StopManaged() {
	managed.mu.Lock()
	defer managed.mu.Unlock()
	if managed.proc == nil {
		return
	}

	pid := managed.proc.Pid
	log.Printf("stopping managed leyline daemon (pid=%d)", pid)

	// SIGTERM: leyline's signal handler unmounts NFS then exits
	if err := managed.proc.Signal(syscall.SIGTERM); err != nil {
		// Process already dead — just clean up
		_ = managed.proc.Release()
		managed.proc = nil
		managed.sock = ""
		return
	}

	// Wait up to 3s for graceful exit
	done := make(chan struct{})
	go func() {
		_, _ = managed.proc.Wait()
		close(done)
	}()

	select {
	case <-done:
		log.Printf("leyline daemon (pid=%d) exited gracefully", pid)
	case <-time.After(3 * time.Second):
		log.Printf("leyline daemon (pid=%d) did not exit after SIGTERM, sending SIGKILL", pid)
		_ = managed.proc.Kill()
		<-done
	}

	managed.proc = nil
	managed.sock = ""
}

// findExistingSocket checks env var and well-known path for an existing socket.
func findExistingSocket() (string, error) {
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

	return "", fmt.Errorf("no socket found")
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
	line, err := c.sendRaw(req)
	if err != nil {
		return nil, err
	}
	var resp map[string]any
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	return resp, nil
}

// SendOpInto sends a JSON request and decodes the response into dest.
// Use this for ops with typed response structs (see wire.go); SendOp's
// map[string]any return silently zeros Int64 fields under the post-b0ea2e
// daemon wire (capnp-json codec emits int64 as JSON strings).
func (c *SocketClient) SendOpInto(req map[string]any, dest any) error {
	line, err := c.sendRaw(req)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(line, dest); err != nil {
		return fmt.Errorf("unmarshal response: %w", err)
	}
	return nil
}

func (c *SocketClient) sendRaw(req map[string]any) ([]byte, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	data = append(data, '\n')

	// Serialize the write-then-read pair. Without this lock, concurrent
	// callers interleave their writes and crossed reads on the line-
	// delimited JSON protocol — caller A reads caller B's response and
	// vice versa, and there's no per-message correlation ID to detect
	// the swap. Pinned by TestSendOp_ConcurrentCallsDoNotInterleave.
	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	if _, err := c.conn.Write(data); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	line, err := c.rd.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	return []byte(strings.TrimSpace(line)), nil
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

// Subscribe sends a subscribe op and returns a channel that receives pushed
// events from the daemon. The subscription runs until the connection closes.
//
// Topics use dot-separated hierarchical matching:
//   - "daemon.snapshot" — exact match
//   - "daemon.*" — one segment wildcard
//   - "daemon.**" — any segments wildcard
//
// The returned channel is buffered (64). If the consumer falls behind,
// events are dropped (the daemon handles overflow per its policy).
func (c *SocketClient) Subscribe(topics []string) (<-chan map[string]any, error) {
	// Send subscribe request.
	req := map[string]any{
		"op":     "subscribe",
		"topics": topics,
	}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal subscribe: %w", err)
	}
	data = append(data, '\n')

	if _, err := c.conn.Write(data); err != nil {
		return nil, fmt.Errorf("write subscribe: %w", err)
	}

	// Read the subscribe response (may include replay events on subsequent lines).
	line, err := c.rd.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("read subscribe response: %w", err)
	}

	var resp map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &resp); err != nil {
		return nil, fmt.Errorf("unmarshal subscribe response: %w", err)
	}
	if errMsg, ok := resp["error"]; ok {
		return nil, fmt.Errorf("subscribe: %v", errMsg)
	}

	ch := make(chan map[string]any, 64)

	// Read pushed events in a goroutine. A 60s read deadline detects
	// daemon crashes that don't cleanly close the socket (e.g., SIGKILL).
	// The deadline resets on every successful read or timeout-with-retry.
	go func() {
		defer close(ch)
		const readTimeout = 60 * time.Second
		for {
			_ = c.conn.SetReadDeadline(time.Now().Add(readTimeout))
			evLine, err := c.rd.ReadString('\n')
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					// Deadline hit with no data — connection likely dead.
					// Could add a ping/pong here later; for now, close.
					return
				}
				return // connection closed or broken
			}
			evLine = strings.TrimSpace(evLine)
			if evLine == "" {
				continue
			}

			var ev map[string]any
			if err := json.Unmarshal([]byte(evLine), &ev); err != nil {
				continue
			}

			// Only forward pushed events (have "event": true).
			if isEvent, _ := ev["event"].(bool); !isEvent {
				continue
			}

			select {
			case ch <- ev:
			default:
				// Drop if consumer is behind.
			}
		}
	}()

	return ch, nil
}

// Prioritize requests the daemon to parse the given files on the next pass.
// Non-blocking from the daemon's perspective: it acknowledges the request
// and schedules the files for priority parsing asynchronously.
func (c *SocketClient) Prioritize(files []string) error {
	_, err := c.SendOp(map[string]any{
		"op":    "prioritize",
		"files": files,
	})
	return err
}

// leylineReleaseURLTemplate is the GitHub releases URL for the public
// ley-line-open repository. The earlier private-repo URL (agentic-research/ley-line)
// was retired when mache integrated against ley-line-open per the T8 work.
//
// Pulls from `/releases/latest/download/` so installs track the latest
// LLO release automatically. The bundle deployment path (apko + melange
// image) ships leyline alongside mache and does not exercise this flow.
// CI can set MACHE_NO_LEYLINE=1 to skip download entirely.
const leylineReleaseURLTemplate = "https://github.com/agentic-research/ley-line-open/releases/latest/download/%s"

// downloadLeyline fetches the leyline binary from the latest GitHub release
// to the specified path. Returns the path on success.
func downloadLeyline(destPath string) (string, error) {
	osName := runtime.GOOS // "darwin" or "linux"
	arch := runtime.GOARCH // "arm64" or "amd64"
	assetName := fmt.Sprintf("leyline-%s-%s", osName, arch)
	url := fmt.Sprintf(leylineReleaseURLTemplate, assetName)

	log.Printf("downloading leyline binary from %s", url)

	resp, err := http.Get(url) //nolint:gosec // URL is hardcoded to GitHub releases
	if err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 404 = "no releases published yet on ley-line-open". This is the
	// expected steady state until LLO cuts its first release. The error
	// is still returned (DiscoverOrStart's caller decides what to do),
	// but with explicit "no release available" wording so logs/UI can
	// distinguish "not published yet" from "real download failure" and
	// optionally degrade gracefully (e.g. fall back to CGO tree-sitter).
	// Bundle deployments don't exercise this code path (leyline ships in
	// the image); CI can set MACHE_NO_LEYLINE=1 to skip download entirely.
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("no leyline release available yet on ley-line-open (HTTP 404 from %s)", url)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}

	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return "", fmt.Errorf("create dir: %w", err)
	}

	// Write to temp file then rename (atomic)
	tmp, err := os.CreateTemp(filepath.Dir(destPath), "leyline-download-*")
	if err != nil {
		return "", fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("write: %w", err)
	}
	_ = tmp.Close()

	// Make executable
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("chmod: %w", err)
	}

	// Atomic rename
	if err := os.Rename(tmpPath, destPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("rename: %w", err)
	}

	log.Printf("leyline binary installed to %s", destPath)
	return destPath, nil
}
