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
	"strconv"
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

// DaemonResponseError is an error envelope returned by the ley-line daemon.
//
// Its message is kept separately so graph adapters can retain their public
// error contracts (for example, mapping a missing node to graph.ErrNotFound)
// while SendOpInto still rejects an unsuccessful typed response loudly.
type DaemonResponseError struct {
	Message string
}

func (e *DaemonResponseError) Error() string {
	return fmt.Sprintf("daemon response: %s", e.Message)
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

// managedDaemon owns the lifecycle of the leyline daemon subprocess
// auto-spawned by mache. At most one per process, cleaned up on exit.
//
// ALL termination goes through its methods (signalGroup / discard) — no
// caller signals .proc directly. The daemon is spawned as a process-group
// leader (setProcessGroup) and these methods signal the whole GROUP, so
// the `mache serve --control` child the daemon spawns is reaped with it
// rather than orphaned (mache-823d91). Routing every kill site through one
// seam is what keeps that discipline from drifting as sites are added;
// TestManagedDaemon_AllTerminationViaSeam enforces it.
type managedDaemon struct {
	mu   sync.Mutex
	proc *os.Process
	sock string
}

var managed managedDaemon

// signalGroup delivers sig to the daemon's whole process group. Caller
// must hold mu. Returns the underlying error (e.g. ESRCH if the group is
// already gone) so callers can branch on a dead daemon.
func (m *managedDaemon) signalGroup(sig syscall.Signal) error {
	return signalProcessGroup(m.proc, sig)
}

// discard SIGKILLs the daemon's group, removes its socket file, and clears
// state. Caller must hold mu. The immediate, non-graceful counterpart to
// StopManaged — used when a managed daemon is stale or failed to start.
func (m *managedDaemon) discard() {
	if m.proc != nil {
		_ = signalProcessGroup(m.proc, syscall.SIGKILL)
	}
	if m.sock != "" {
		_ = os.Remove(m.sock)
	}
	m.proc = nil
	m.sock = ""
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
		// Stale managed daemon: reap its group + clear so the spawn block
		// re-runs. discard() does the group-kill that avoids orphaning the
		// daemon's mache child.
		managed.discard()
	}

	// Find the leyline binary through the SAME pin-checked chokepoint as
	// the build path (ResolveBinary: PATH → ~/.mache/bin → SHA-verified
	// download, each candidate accepted only on the exact pinned version).
	// This block previously did a raw LookPath + unchecked cache stat —
	// the drift ResolveBinary was built to kill (mache-0acdf6): a stale
	// PATH leyline would be SPAWNED as the daemon, and with the write-back
	// validate op requiring >= v0.7.8 every write would draft with a
	// daemon-too-old diagnostic. An already-RUNNING daemon (fast paths
	// above) is still honored as-is — it is the user's own; the wire
	// handshake and per-op probes cover that case. ResolveBinary respects
	// MACHE_NO_LEYLINE before downloading.
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	leylineBin, err := ResolveBinary(true)
	if err != nil {
		return "", fmt.Errorf("no pinned leyline to spawn: %w", err)
	}
	source := "resolved-pinned"
	// Record which binary won, from which tier, at what version — so the
	// resolution is visible in logs AND via get_sheaf_status (mache-0dcd98).
	// This was invisible before, which made the stale-cached-0.4.1 skew
	// (mache-0acdf6) an archaeology dig instead of a one-line answer.
	recordResolvedLeyline(leylineBin, source)
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
	daemonArgs := []string{
		"daemon",
		"--arena", arenaPath,
		"--arena-size-mib", "64",
		"--control", ctrlPath,
		"--mount", mountDir,
	}
	// --source lets the daemon run enrichment passes: op_enrich requires
	// ctx.source_dir, so without it lsp/embed enrichment fails with "no
	// --source configured". Set via SetDaemonSource at serve startup when
	// serving a source tree; empty for pre-baked .db serves (mache-303036).
	if src := DaemonSource(); src != "" {
		daemonArgs = append(daemonArgs, "--source", src)
	}
	// CDC is opt-in for Mache-managed daemons only. Existing sockets and
	// external --control daemons retain the configuration chosen by their
	// operator; a live daemon cannot change startup arguments.
	if DaemonCDC() {
		daemonArgs = append(daemonArgs, "--cdc")
	}
	cmd := exec.Command(leylineBin, daemonArgs...)
	// Detach from our stdio so it doesn't interfere with MCP transport
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil

	// Run the daemon as a process-group leader so that the `mache serve
	// --control` child it spawns is reaped together with it — otherwise
	// killing the daemon (timeout below, or StopManaged) orphans that
	// child, which accumulates and contends for the shared arena/socket
	// (mache-823d91).
	setProcessGroup(cmd)

	log.Printf("auto-starting leyline daemon: %s", strings.Join(cmd.Args, " "))
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start leyline: %w", err)
	}

	managed.proc = cmd.Process
	managed.sock = sockPath

	// Wait on the process in the background (prevents a zombie) and surface
	// its exit to the poll below so we can distinguish "daemon crashed on
	// startup" from "daemon is still initializing" (mache-0a1ded). Buffered
	// so the send never blocks after the poll returns.
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	// Poll for socket to appear AND accept a connection. Mere os.Stat is
	// insufficient — the kernel can create the inode before the daemon
	// finishes its accept loop bind, and a stat-only wait would return a
	// path that DialSocket can't connect to.
	//
	// The wait is configurable (MACHE_LEYLINE_START_TIMEOUT): a cold start —
	// first run, arena init, enrichment setup, or contention with co-tenant
	// daemons on the shared arena — can exceed the old fixed 5s.
	timeout := leylineStartTimeout()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if isSocketAlive(sockPath) {
			log.Printf("leyline daemon ready (pid=%d, socket=%s)", cmd.Process.Pid, sockPath)
			return sockPath, nil
		}
		// If the daemon already exited, the socket will never appear — stop
		// waiting and report it as a crash, not a timeout.
		select {
		case werr := <-waitErr:
			managed.discard()
			if werr == nil {
				// Clean exit (status 0) before binding is still a failure —
				// nothing is listening on the socket. Return an explicit error
				// rather than wrapping nil (which renders "%!w(<nil>)").
				return "", fmt.Errorf("leyline daemon exited cleanly (status 0) during startup before socket %s appeared", sockPath)
			}
			return "", fmt.Errorf("leyline daemon exited during startup before socket %s appeared: %w", sockPath, werr)
		default:
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Timed out with the process still running — initializing too slowly or
	// wedged. Discard the group (daemon + any child it spawned) and point at
	// the knob to extend the wait and the likely-contended arena.
	managed.discard()
	return "", fmt.Errorf("leyline daemon started but socket %s did not appear within %s — it may still be initializing or contending for %s; raise MACHE_LEYLINE_START_TIMEOUT to allow longer",
		sockPath, timeout, arenaPath)
}

// defaultLeylineStartTimeout is how long DiscoverOrStart waits for a freshly
// spawned daemon's socket to accept connections. Raised from the original
// fixed 5s (mache-0a1ded): a cold start — first run, arena init, enrichment
// setup, or contention with co-tenant daemons on the shared
// ~/.mache/default.arena — routinely needs longer.
const defaultLeylineStartTimeout = 15 * time.Second

// leylineStartTimeout returns the socket-appear wait, overridable via
// MACHE_LEYLINE_START_TIMEOUT (a Go duration like "30s", or a bare integer
// interpreted as seconds). Invalid or non-positive values fall back to the
// default.
func leylineStartTimeout() time.Duration {
	v := strings.TrimSpace(os.Getenv("MACHE_LEYLINE_START_TIMEOUT"))
	if v == "" {
		return defaultLeylineStartTimeout
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return defaultLeylineStartTimeout
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

	// SIGTERM the whole group: leyline's signal handler unmounts NFS then
	// exits, and the `mache serve --control` child it spawned is terminated
	// alongside it rather than orphaned (mache-823d91).
	if err := managed.signalGroup(syscall.SIGTERM); err != nil {
		// Group already gone — just clean up
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
		_ = managed.signalGroup(syscall.SIGKILL)
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
	var envelope struct {
		Error *string `json:"error"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		return fmt.Errorf("unmarshal response envelope: %w", err)
	}
	if envelope.Error != nil {
		return &DaemonResponseError{Message: *envelope.Error}
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

// Enrich runs a daemon enrichment pass (e.g. "lsp", "embed") over the
// given files via the `enrich` op, populating the corresponding arena
// tables (for "lsp": _lsp_hover / _lsp / _lsp_defs / _lsp_refs). `files`
// are paths relative to the daemon's --source dir; nil/empty enriches every
// file. The op is SYNCHRONOUS on the daemon side — it runs the pass and
// snapshots the living db to the arena before responding — so a subsequent
// Query observes the populated tables.
//
// This replaces the prior `tool` op: the leyline daemon's UDS dispatch
// speaks named ops directly ({"op":"enrich","pass":...}); the MCP
// tools/call envelope ({"op":"tool","name":...}) only exists on the HTTP
// /mcp endpoint, so sending it over UDS returned "unknown op: tool"
// (mache-303036).
func (c *SocketClient) Enrich(pass string, files []string) (map[string]any, error) {
	req := map[string]any{"op": "enrich", "pass": pass}
	if len(files) > 0 {
		req["files"] = files
	}
	resp, err := c.SendOp(req)
	if err != nil {
		return nil, err
	}
	if errMsg, ok := resp["error"]; ok {
		return nil, fmt.Errorf("enrich %s: %v", pass, errMsg)
	}
	return resp, nil
}

// Query runs a SQL query against the active arena buffer via the `query` op
// and returns the rows as [][]any (one slice per row, column order matching
// the SELECT / the response's `columns` field).
//
// The daemon's `query` op returns each row as a JSON OBJECT keyed by column
// name ({"node_id":..,"hover_text":..}), with a separate ordered `columns`
// array — NOT an array of values. (A legacy array-of-arrays shape is also
// accepted for forward/back-compat.) The prior parser only handled the
// array shape and silently dropped every object row, so any query against
// the daemon returned zero rows — which is exactly why LSP get_type_info
// surfaced no hover even though enrichment populated _lsp_hover (mache-303036).
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
	rowsVal, hasRows := resp["rows"]
	if !hasRows {
		return nil, nil
	}
	rawRows, ok := rowsVal.([]any)
	if !ok {
		return nil, fmt.Errorf("unexpected rows type: %T", rowsVal)
	}
	// Ordered column names — used to flatten object-shaped rows in SELECT order.
	var cols []string
	if cv, ok := resp["columns"].([]any); ok {
		cols = make([]string, len(cv))
		for i, v := range cv {
			cols[i] = fmt.Sprint(v)
		}
	}
	result := make([][]any, 0, len(rawRows))
	for _, r := range rawRows {
		switch row := r.(type) {
		case []any: // legacy array-of-values shape
			result = append(result, row)
		case map[string]any: // daemon shape: object keyed by column name
			ordered := make([]any, len(cols))
			for i, col := range cols {
				ordered[i] = row[col]
			}
			result = append(result, ordered)
		}
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
// when no leyline binary is found on PATH or in ~/.mache/bin/. This tracks the
// latest published ley-line-open release that ships platform binaries (only
// tagged releases have downloadable leyline-<os>-<arch> assets — the go.mod
// schema-client pin may sit on a newer pseudo-version that has no release).
//
// As of this pin, both the daemon binary and go.mod's leyline-schema module are
// v0.15.1. The wire major (1) and compatibility floor (0.6.0) are unchanged
// from the 0.10 line; the version-parity gate (mache-b8af69) enforces the
// [floor, binary] range.
//
// v0.13.0 -> v0.15.1 DOES NOT CROSS AN IR LINEAGE BOUNDARY — checked against
// LLO's own compatibility.json at each tag: ir_schema_version reads
// "merkle-ast-v2" and wire_format_major reads 1 at v0.13.0, v0.14.0, v0.15.0,
// and v0.15.1 alike. No .db rebuild is required crossing this bump.
//
// The three intervening releases (v0.14.0 "the signing train": DSSE/in-toto
// envelope, wasm32 artifacts, `leyline self install/update`; v0.15.0
// "execution/v1": tier ceilings, the confinement manifest + attested digest;
// v0.15.1: fixes the v0.15.0 confinement digest so it actually commits to the
// policy) are all execution/confinement/signing surface. None of it touches
// the _ast/node_refs/node_defs projection mache actually consumes — verified
// by grepping each release's CHANGELOG entry for _ast/node_refs/schema
// mentions (none) and by the unchanged ir_schema_version/wire_format_major
// above, not by assuming "sounds orthogonal" is enough.
//
// MARKDOWN BACKTICK SPANS BECOME node_refs (ley-line-open-ea1e42, mache
// bead mache-eb2bf3). Every markdown `inline` node now reparses under
// tree-sitter-md's INLINE_LANGUAGE via the existing injection mechanism;
// each code_span emits a node_refs row on the HOST .md file (delimiters
// stripped, CommonMark padding trimmed, verbatim otherwise) — join
// node_refs, not _ast, per LLO's own correction on mache-eb2bf3 (their first
// answer said _ast-only; the injection fold deliberately emits no _ast rows
// for injected subtrees, so the real channel is node_refs facts on the host
// file). container_node_id is NULL for these rows — a doc citation has no
// enclosing function — so any rule that aggregates v_refs by referrer must
// account for that (mache-50e939 found and fixed fan_out_skew's corpus mean
// being diluted by the resulting singleton referrer groups; audit any other
// AVG-over-v_refs rule before relying on it here). Host _ast/node_hash are
// byte-identical with the pass on or off (pinned by LLO's own test).
// drift_doc_dead_symbol_reference (mache's 0.20 placeholder) is now
// writable: node_refs rows from *.md sources whose token joins no living
// def. Caveat carried from LLO: ley-line-open-651909 (Go package-level
// consts emit no defs, qualified selector args emit no refs) is still open
// and will false-positive a Go-scoped doc-drift join — scope to Rust
// symbols first.
//
// v0.11.3 -> v0.12.1 DID NOT CROSS AN IR LINEAGE BOUNDARY either — same
// measurement, same fixture, ir_schema_version and extraction_epoch both
// held (250369 -> 250369 _ast rows on re-parse). ley-line-open-348de6
// shipped the ir_schema_version field in v0.11.1 — the first mache bump able
// to answer the lineage question this way instead of reading changelog
// prose; see mache-43d63d for consuming it as a first-class signal rather
// than reading it ad hoc as done here.
//
// v0.12.x added leyline-mcp-descriptor, a shared server.json emitter
// (ley-line-open-4ec276) that mache's own server.json work fed into
// (mache-e28a9f): multiple packages / per-package transport landed
// (ley-line-open-44cc45), but there is still no session field on
// TransportMeta, so cloister cannot yet derive mache's per-transport session
// requirement (http is stateful, stdio is not) from the generated descriptor.
//
// THE 0.10 -> 0.11 BUMP CROSSES AN IR LINEAGE BOUNDARY. v0.11.0 raised
// IR_SCHEMA_VERSION from "merkle-ast-v1" to "merkle-ast-v2": giving
// function_signature_item the canonical_kind `function` it lacked rewrote every
// Rust trait signature's node_hash, because the preimage hashes
// canonical_kind(raw).unwrap_or(raw). SOURCES ARE BYTE-IDENTICAL, so no
// content- or time-based check can see it — mache's cache lockfiles hash raw
// source bytes and the parse skip is mtime+size. Any persisted .db built by a
// pre-0.11 leyline must be REBUILT, not reused; a stale one serves v1 addresses
// while this binary computes v2 (mache-438104). _mache_meta now records
// leyline_pin / leyline_version so which-leyline-built-this is answerable from
// the artifact.
//
// v0.11.x also changed what the projection CONTAINS, which is why the smell
// baseline was regenerated in the same commit rather than separately:
//   - module qualification (ley-line-open-23377a) emits a qualified alias
//     alongside each bare token, doubling def rows under mod_item (966 -> 1932
//     on the Rust snapshot). The definitions are unchanged — distinct node_id
//     is 7251 before and after — but the token vocabulary is not, so
//     duplicate_definitions legitimately reports different groups.
//   - overloaded symbols survive (ley-line-open-5d3cb6): _lsp previously kept
//     one row per {parent}/{name} and silently replaced the rest. Now
//     discernible overloads carry a signature discriminator and indistinct ones
//     an ordinal. NOTE the ordinal branch is positional — verified that
//     inserting a TypeScript overload renumbers the ones below it — so #~N is
//     not a stable address.
//
// Earlier context: v0.7.0 raised the compatibility floor to 0.6.0 and added
// source_blobs / capnp_blobs / _ast_pointer plus the unified
// daemon.sheaf.invalidate topic. v0.7.4 and v0.7.5 added AST columns without
// changing the wire major; v0.8.0 added validate coverage, node_refs.qualifier,
// and extraction epochs.
//
// The version-check (leylineVersionMatchesPin) is EXACT major.minor.patch —
// LLO patch releases have changed the emitted _ast schema (0.7.4 added
// container_node_id, 0.7.5 added canonical_kind), so the patch does NOT
// float (mache-608a3c). v0.11.3 ships all four leyline-<os>-<arch> daemon
// binaries (darwin/linux x amd64/arm64).
//
// BUMP THIS to the latest published ley-line-open release with binary assets.
// The go.mod leyline-schema pin only needs bumping when a schema module tag is
// published. The parity gate enforces the [floor, binary] compatibility range.
//
// Doubles as this build's leyline schema-client version for the startup
// wire-compat handshake (VerifyReachableDaemonVersion, mache-8kif): mache
// queries the daemon's leyline_version op and refuses on a structural
// mismatch.
const leylineBinaryVersion = "v0.15.1"

// leylineSchemaCompatFloor is the OLDEST leyline-schema Go client version
// whose wire format the pinned binary still accepts (ley-line-open's
// compat_min_schema_version). The schema Go module
// (clients/go/leyline-schema, go.mod pinned) is tagged SEPARATELY from the
// binary. The parity gate asserts the go.mod schema pin sits in [floor, binary]
// rather than requiring equality, since compatible binary-only releases may
// legitimately advance without a new schema tag. BUMP this when ley-line-open
// raises compat_min_schema_version. (mache-b8af69 / mache-dcb808)
const leylineSchemaCompatFloor = "v0.6.0"

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
// ResolveBinary locates a usable leyline binary and returns its path.
// Resolution order: PATH → ~/.mache/bin/leyline → (when allowDownload)
// auto-download the pinned ley-line-open release. This is the single
// provisioning chokepoint shared by the serve path (autoInvokeLeylineParse)
// and the build path, so `mache build` and `mache serve` acquire leyline
// identically instead of the build path silently degrading to tree-sitter.
//
// Auto-download is skipped — returning an error rather than fetching — when
// allowDownload is false, or when MACHE_NO_LEYLINE is set (the CI/offline
// opt-out). A downloaded binary is cached at ~/.mache/bin/leyline for reuse.
func ResolveBinary(allowDownload bool) (string, error) {
	// Explicit developer override first (MACHE_LEYLINE_BINARY). Checked before
	// every pinned tier so it is a decision, not a fallback: if it is set and
	// broken we fail rather than quietly resolving something else.
	if p, set, err := overrideBinary(); set {
		if err != nil {
			return "", err
		}
		return p, nil
	}
	// A leyline on PATH is used ONLY if it is the pinned version. A stale local
	// install (the recurring "0.5.7 shadows the pin" trap) or a raw-main build
	// reports a different version and produces different _ast output than the
	// pinned release — silently diverging local runs from CI. Reject it and fall
	// through rather than trust whatever happens to be on PATH.
	if p, err := exec.LookPath("leyline"); err == nil {
		if leylineVersionMatchesPin(p) {
			return p, nil
		}
		log.Printf("leyline on PATH (%s) is not the pinned %s — ignoring it; resolving the pinned binary", p, leylineBinaryVersion)
	}
	// Version-namespaced cache: a build pinning a different version keeps its
	// own file, so concurrent pins cannot overwrite each other (see
	// binary_cache_path.go).
	bundled, err := pinnedCachePath()
	if err != nil {
		return "", err
	}
	if cached := resolveCachedPinned(); cached != "" {
		return cached, nil
	}
	if !allowDownload {
		return "", fmt.Errorf("no leyline matching the pinned %s found on PATH or at %s (auto-download disabled)", leylineBinaryVersion, bundled)
	}
	if os.Getenv("MACHE_NO_LEYLINE") != "" {
		return "", fmt.Errorf("no leyline matching the pinned %s available and MACHE_NO_LEYLINE is set; install leyline %s or unset MACHE_NO_LEYLINE to auto-download", leylineBinaryVersion, leylineBinaryVersion)
	}
	return downloadLeyline(bundled)
}

// EnsureCachedBinary returns the exact pinned Leyline release from
// ~/.mache/bin, downloading and SHA-verifying it when the cache is absent or
// stale. Unlike ResolveBinary, this deliberately does not consult PATH:
// conformance tests need to exercise the published artifact rather than a
// same-version developer build.
func EnsureCachedBinary() (string, error) {
	cached, err := pinnedCachePath()
	if err != nil {
		return "", err
	}
	if hit := resolveCachedPinned(); hit != "" {
		return hit, nil
	}
	log.Printf("no cached leyline matching the pinned %s — fetching published artifact to %s", leylineBinaryVersion, cached)
	if os.Getenv("MACHE_NO_LEYLINE") != "" {
		return "", fmt.Errorf("no cached leyline matching the pinned %s available and MACHE_NO_LEYLINE is set", leylineBinaryVersion)
	}
	return downloadLeyline(cached)
}

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

	// Supply-chain integrity: the downloaded bytes must match the pinned
	// release SHA-256 before we install or run them (mache-46af85).
	if err := verifyLeylineSHA256(tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}

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
