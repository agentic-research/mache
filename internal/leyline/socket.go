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
	"sync/atomic"
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

	// subscribeDropped counts events the Subscribe goroutine had to drop
	// because the consumer wasn't reading fast enough. Exposed via
	// SubscribeDropped(); the goroutine logs the first drop and every
	// 100th drop thereafter so operators can spot a stuck consumer
	// without flooding the log.
	subscribeDropped atomic.Uint64

	// subscribeParseFailures counts *consecutive* malformed event lines.
	// The goroutine resets it to zero on every successful parse and
	// returns (closing the channel) if it stays above
	// maxConsecutiveParseFailures — sustained garbage on the wire is
	// treated as a dead/corrupt connection rather than transient noise.
	subscribeParseFailures atomic.Uint64

	// closeOnce gates Close so the underlying conn.Close runs exactly
	// once, even under concurrent shutdown (SheafSubscriber.run calls
	// sock.Close while the Subscribe read goroutine may still be in
	// its SetReadDeadline + ReadString loop). The prior shape — guard
	// with `if c.conn != nil` then assign `c.conn = nil` — was a real
	// data race surfaced by PR #384 CI (run 26063835518, macos-latest):
	// the Subscribe goroutine read c.conn at socket.go:433 while
	// Close raced the nil-out at socket.go:267, then deref'd nil.
	// sync.Once removes the write entirely; the closed conn pointer
	// stays valid and the goroutine exits cleanly via the closed-conn
	// error path from SetReadDeadline / ReadString.
	closeOnce sync.Once
	closeErr  error
}

// SubscribeDropped returns the cumulative number of events the Subscribe
// goroutine has dropped because the consumer's channel buffer was full.
// Non-zero values mean the consumer is falling behind the daemon's event
// rate; investigate before treating cache state as authoritative.
func (c *SocketClient) SubscribeDropped() uint64 {
	return c.subscribeDropped.Load()
}

// maxConsecutiveParseFailures is the threshold at which the Subscribe
// goroutine gives up and closes the channel. Sustained malformed lines
// almost always mean the connection is desynced or talking to the wrong
// protocol; one-off blips (single corrupted line) shouldn't kill the
// subscription. 16 is high enough that JSON-coding hiccups don't bite,
// low enough that the consumer notices within a second on a noisy stream.
const maxConsecutiveParseFailures = 16

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

// socketLivenessTimeout bounds the connect-test used to decide whether a
// discovered socket file is backed by a live daemon. 500ms is generous
// enough to absorb a busy daemon's accept-loop scheduling (the daemon's
// accept must run before the connect returns) and short enough that a
// stale post-SIGKILL socket — which fails immediately with ECONNREFUSED
// because nothing is listening — adds no noticeable startup cost.
const socketLivenessTimeout = 500 * time.Millisecond

// isSocketAlive returns true iff a Unix-domain dial to sockPath succeeds
// within socketLivenessTimeout. A "yes" means *something* is accepting
// connections on the path; we don't speak the protocol here. A "no"
// covers both the missing-file and stale-file-no-listener cases, which
// is the discrimination DiscoverOrStart needs to avoid orphaning a fresh
// daemon onto a path whose previous owner was SIGKILL'd.
func isSocketAlive(sockPath string) bool {
	conn, err := net.DialTimeout("unix", sockPath, socketLivenessTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
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
//
// Dedup (mache-52a23a): every "is there already a socket?" check verifies
// the socket is *live* (a Unix dial connects) before returning the path.
// A stale socket file left behind by a SIGKILL'd daemon would otherwise
// be returned by findExistingSocket and cause the caller to fail at first
// DialSocket. Worse, *between* the stat and the spawn, the entire managed
// daemon path can also be re-entered, orphaning daemons under concurrent
// callers within the same process. The full DiscoverOrStart now runs
// under managed.mu so the discover-then-spawn sequence is atomic across
// goroutines; stale on-disk sockets are removed before spawning so the
// daemon can bind the well-known path cleanly.
func DiscoverOrStart() (string, error) {
	// Take the managed-state lock for the full discover-then-spawn cycle.
	// Prior code released the lock after the in-memory managed check, so
	// two goroutines that both observed managed.sock == "" could each
	// proceed to cmd.Start() before either wrote managed.proc — a clean
	// double-spawn that orphans one daemon (the second binder loses the
	// socket race, exits unnoticed). Holding through Start fixes that.
	managed.mu.Lock()
	defer managed.mu.Unlock()

	// Fast path 1: env var or well-known path resolves AND the socket is
	// live. A bare os.Stat hit isn't enough — a post-SIGKILL daemon
	// leaves the socket file behind with nothing listening, and the
	// caller's first SendOp would fail with "connect: connection refused"
	// long after this returned "found it!".
	if sock, err := findExistingSocket(); err == nil {
		if isSocketAlive(sock) {
			return sock, nil
		}
		// Stale: file exists, no listener. Remove it so the spawn below
		// can re-bind the well-known path. Only the well-known path is
		// owned by mache; an explicit LEYLINE_SOCKET points at someone
		// else's daemon and we must not touch it.
		if env := os.Getenv("LEYLINE_SOCKET"); env == "" {
			_ = os.Remove(sock)
		} else {
			return "", fmt.Errorf("LEYLINE_SOCKET=%s exists but no daemon is listening", env)
		}
	}

	// Fast path 2: previously spawned managed daemon in this process. The
	// liveness probe re-checks here too — managed.proc might have died
	// since we last looked (e.g. OOM-killed independently of mache).
	if managed.sock != "" {
		if isSocketAlive(managed.sock) {
			return managed.sock, nil
		}
		// Stale managed daemon: reap and reset so the spawn block runs.
		if managed.proc != nil {
			_ = managed.proc.Kill()
		}
		_ = os.Remove(managed.sock)
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

	// Last-chance pre-spawn liveness check on the would-be socket path.
	// A concurrent mache invocation (different process) might have just
	// bound this exact path between findExistingSocket and here; if so,
	// piggyback rather than starting a second daemon that will fight for
	// the same arena/control files.
	//
	// coverage:ignore — defensive TOCTOU guard against an external
	// process binding sockPath between findExistingSocket (L179) and
	// here. Cannot be exercised hermetically in-process: if a listener
	// is alive at sockPath when findExistingSocket runs, fast-path 1
	// returns at L181; if it isn't, only an external second-process
	// race can flip isSocketAlive to true here. The same path is
	// safety-netted by the post-spawn poll loop (L290-297) if a
	// concurrent spawn lands on the same inode.
	if isSocketAlive(sockPath) { // coverage:ignore
		return sockPath, nil // coverage:ignore
	} // coverage:ignore
	// And clear any leftover socket inode so the daemon can bind.
	_ = os.Remove(sockPath)

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

	// Poll for socket to appear AND accept a connection. Mere os.Stat is
	// insufficient — the kernel can create the inode before the daemon
	// finishes its accept loop bind, and a stat-only wait would return a
	// path that DialSocket can't connect to.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if isSocketAlive(sockPath) {
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

// Close closes the underlying connection. Safe to call multiple times
// from any number of goroutines — the sync.Once gates the underlying
// conn.Close so concurrent shutdown can't race the Subscribe read
// goroutine (see closeOnce docstring on SocketClient for the history).
// The conn pointer is intentionally NOT zeroed; closed conn errors
// from subsequent reads/writes propagate normally to their callers.
func (c *SocketClient) Close() error {
	c.closeOnce.Do(func() {
		if c.conn != nil {
			c.closeErr = c.conn.Close()
		}
	})
	return c.closeErr
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
	if err != nil { // coverage:ignore — map[string]any with only string + []string values cannot fail json.Marshal; the branch is defensive-only
		return nil, fmt.Errorf("marshal subscribe: %w", err) // coverage:ignore
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
	go c.runSubscribeLoop(ch, defaultSubscribeReadTimeout)

	return ch, nil
}

// defaultSubscribeReadTimeout is the read deadline for pushed events on a
// live subscription. Long enough that an idle subscription doesn't churn,
// short enough that a daemon SIGKILL is detected before the consumer makes
// stale cache decisions. Overridable via runSubscribeLoop for tests so we
// don't have to wait 60s to exercise the timeout path.
const defaultSubscribeReadTimeout = 60 * time.Second

// runSubscribeLoop is the body of the Subscribe goroutine, factored out
// so tests can drive the loop with a short readTimeout without actually
// waiting the production 60s.
//
// Closes ch when the connection drops, the read deadline expires, or
// the wire is so persistently malformed (maxConsecutiveParseFailures+1
// bad lines in a row) that we treat it as dead.
func (c *SocketClient) runSubscribeLoop(ch chan<- map[string]any, readTimeout time.Duration) {
	defer close(ch)
	for {
		_ = c.conn.SetReadDeadline(time.Now().Add(readTimeout))
		evLine, err := c.rd.ReadString('\n')
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				// Deadline hit with no data — connection likely dead.
				// Log this distinctly from a clean EOF so operators can
				// tell "daemon SIGKILLed (deadline)" apart from "daemon
				// closed cleanly (EOF)" when triaging stale-cache reports.
				// Could add a ping/pong here later; for now, close.
				log.Printf("subscribe: read deadline exceeded, treating connection as dead")
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
			// Log malformed events instead of dropping silently. A single
			// bad line is recoverable (resync on the next \n), but a flood
			// of them means the wire is desynced — bail after
			// maxConsecutiveParseFailures so a corrupted connection
			// doesn't spin forever.
			log.Printf("subscribe: drop malformed event: %v (line=%q)", err, evLine)
			if c.subscribeParseFailures.Add(1) > maxConsecutiveParseFailures {
				log.Printf("subscribe: %d consecutive malformed events, closing subscription", maxConsecutiveParseFailures+1)
				return
			}
			continue
		}
		c.subscribeParseFailures.Store(0)

		// Only forward pushed events (have "event": true).
		if isEvent, _ := ev["event"].(bool); !isEvent {
			continue
		}

		select {
		case ch <- ev:
		default:
			// Consumer is behind — count + log so operators can see it
			// before stale-cache decisions pile up. Log on the first
			// drop (so we don't miss the very first signal) and then
			// every 100 drops to bound log volume on a stuck consumer.
			dropped := c.subscribeDropped.Add(1)
			if dropped == 1 || dropped%100 == 0 {
				log.Printf("subscribe: dropping events (consumer behind); total dropped=%d", dropped)
			}
		}
	}
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

// leylineBinaryVersion pins the ley-line-open release that mache downloads
// when no leyline binary is found on PATH or in ~/.mache/bin/. This MUST
// mirror the schema-client pin in go.mod:
//
//	github.com/agentic-research/ley-line-open/clients/go/leyline-schema v0.4.1
//
// BUMP THIS WHEN UPDATING leyline-schema in go.mod. The Go schema-client and
// the daemon binary travel together — mache built against schema vX.Y.Z must
// run a daemon at the same major/minor or it will mis-decode the wire format.
//
// Future hardening (mache-8kif): on socket connect, query the daemon's
// `version` op and refuse to proceed if it disagrees with this constant.
const leylineBinaryVersion = "v0.4.1"

// leylineReleaseURLTemplate is the GitHub releases URL for the public
// ley-line-open repository. The earlier URL pointed at the private
// agentic-research/ley-line repo (mache-9051f0) and used /releases/latest/
// which floated against whatever the most recent LLO tag happened to be —
// a recipe for picking up a binary that doesn't match mache's schema-client
// pin. Both issues are fixed by pinning to ley-line-open at a specific tag.
//
// The bundle deployment path (apko + melange image) ships leyline alongside
// mache and does not exercise this flow. CI can set MACHE_NO_LEYLINE=1 to
// skip download entirely.
//
// First %s = version tag, second %s = asset name.
//
// Declared as var (not const) so hermetic tests can swap in an httptest server
// URL to exercise downloadLeyline without touching the network. Production
// code never mutates this — see socket_test.go for the test-only override.
var leylineReleaseURLTemplate = "https://github.com/agentic-research/ley-line-open/releases/download/%s/%s"

// downloadLeyline fetches the pinned-version leyline binary from GitHub
// releases (see leylineBinaryVersion) to the specified path. Returns the
// path on success.
func downloadLeyline(destPath string) (string, error) {
	osName := runtime.GOOS // "darwin" or "linux"
	arch := runtime.GOARCH // "arm64" or "amd64"
	assetName := fmt.Sprintf("leyline-%s-%s", osName, arch)
	url := fmt.Sprintf(leylineReleaseURLTemplate, leylineBinaryVersion, assetName)

	log.Printf("downloading leyline binary from %s", url)

	resp, err := http.Get(url) //nolint:gosec // URL is hardcoded to GitHub releases
	if err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 404 = "this pinned version isn't published on ley-line-open" — either
	// the tag doesn't exist, or the per-OS/arch asset wasn't uploaded for
	// this release. With a version-pinned URL (no more /latest/) this is
	// almost always a mismatch between leylineBinaryVersion and the actual
	// LLO releases page. The error is still returned (DiscoverOrStart's
	// caller decides what to do), but with explicit "no release available"
	// wording so logs/UI can distinguish "not published yet" from "real
	// download failure" and optionally degrade gracefully (e.g. fall back
	// to CGO tree-sitter). Bundle deployments don't exercise this code
	// path (leyline ships in the image); CI can set MACHE_NO_LEYLINE=1 to
	// skip download entirely.
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("no leyline release available at pinned version %s on ley-line-open (HTTP 404 from %s)", leylineBinaryVersion, url)
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
