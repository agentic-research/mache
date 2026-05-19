package leyline

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "t.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			go func(c net.Conn) {
				defer c.Close() //nolint:errcheck
				rd := bufio.NewReader(c)
				for {
					line, err := rd.ReadString('\n')
					if err != nil {
						return
					}
					var req map[string]any
					if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &req); err != nil {
						resp, _ := json.Marshal(map[string]any{"error": "bad json"})
						_, _ = c.Write(append(resp, '\n'))
						continue
					}
					resp := handler(req)
					data, _ := json.Marshal(resp)
					_, _ = c.Write(append(data, '\n'))
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
	defer client.Close() //nolint:errcheck

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
	defer client.Close() //nolint:errcheck

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
	defer client.Close() //nolint:errcheck

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
	defer client.Close() //nolint:errcheck

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
	defer client.Close() //nolint:errcheck

	// Set a very short deadline
	_ = client.SetDeadline(time.Now().Add(10 * time.Millisecond))
	_, err = client.SendOp(map[string]any{"op": "status"})
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestDiscoverSocket_EnvVar(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	// Create the file so Stat succeeds
	f, _ := os.Create(sockPath)
	_ = f.Close()

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

func TestDiscoverOrStart_UsesExistingSocket(t *testing.T) {
	// Real UDS listener — after mache-52a23a, DiscoverOrStart probes the
	// socket with a live Dial (not just os.Stat) so a touch-file isn't
	// enough to pass the dedup gate. The listener simulates an existing
	// daemon and must accept the connect attempt.
	dir, err := os.MkdirTemp("/tmp", "leyline-existing-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "test.sock")

	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go acceptAndClose(ln)

	t.Setenv("LEYLINE_SOCKET", sockPath)
	found, err := DiscoverOrStart()
	if err != nil {
		t.Fatalf("DiscoverOrStart: %v", err)
	}
	if found != sockPath {
		t.Errorf("expected %s, got %s", sockPath, found)
	}
}

// acceptAndClose drains accept events from ln until it closes, immediately
// closing each accepted conn. Used to satisfy isSocketAlive() probes
// without speaking the request/response protocol.
func acceptAndClose(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_ = conn.Close()
	}
}

func TestDiscoverOrStart_NoBinaryOnPath(t *testing.T) {
	// Clear env so no existing socket is found
	t.Setenv("LEYLINE_SOCKET", "/tmp/nonexistent-leyline-test.sock")
	// Ensure leyline binary is not on PATH
	t.Setenv("PATH", "/nonexistent-path-for-test")
	// Steer ~/.mache/bin fallback at an empty tempdir so the cached-binary
	// branch is not taken.
	t.Setenv("HOME", t.TempDir())
	// Gate out the auto-download path. Without this gate, ley-line-open's
	// published releases (v0.4.1+) succeed the download, the daemon spawns,
	// and this test no longer exercises the "no binary anywhere" failure
	// mode it was written for.
	t.Setenv("MACHE_NO_LEYLINE", "1")

	_, err := DiscoverOrStart()
	if err == nil {
		t.Fatal("expected error when no socket and no binary")
	}
	if !strings.Contains(err.Error(), "MACHE_NO_LEYLINE") {
		t.Errorf("expected 'MACHE_NO_LEYLINE' in error, got: %v", err)
	}
}

func TestDiscoverOrStart_NoLeylineEnv_SkipsDownload(t *testing.T) {
	// Regression guard: when MACHE_NO_LEYLINE=1 (the documented CI /
	// bundle-deployment switch), DiscoverOrStart must NOT attempt a
	// network download even if leyline is absent from every fallback
	// location. Without this gate a clean-clone CI run can hit a
	// non-deterministic network fetch (potentially 404s from ley-line-
	// open's not-yet-published releases endpoint).
	t.Setenv("LEYLINE_SOCKET", "/tmp/nonexistent-leyline-test.sock")
	t.Setenv("PATH", "/nonexistent-path-for-test")
	// Steer the ~/.mache/bin fallback at a tempdir guaranteed to have
	// no leyline binary, so the auto-download branch is the only path
	// that the test could fall into.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MACHE_NO_LEYLINE", "1")

	_, err := DiscoverOrStart()
	if err == nil {
		t.Fatal("expected error when MACHE_NO_LEYLINE is set and no leyline available")
	}
	if !strings.Contains(err.Error(), "MACHE_NO_LEYLINE") {
		t.Errorf("expected 'MACHE_NO_LEYLINE' in error, got: %v", err)
	}
	// Negative assertion: the error must NOT mention a download attempt —
	// that would mean the env var didn't gate the download path.
	if strings.Contains(err.Error(), "auto-download failed") || strings.Contains(err.Error(), "no leyline release available") {
		t.Errorf("MACHE_NO_LEYLINE should short-circuit BEFORE downloadLeyline; got: %v", err)
	}
}

func TestStopManaged_SafeWhenNoDaemon(t *testing.T) {
	// Should not panic when no managed daemon exists
	StopManaged()
}

func TestLeylineReleaseURL_PointsAtPublicRepo(t *testing.T) {
	// Regression guard for mache-9051f0: the auto-download URL must point
	// at the public ley-line-open repo, not the private ley-line one. The
	// earlier URL pre-dated the public/private split; consumers without
	// access to the private repo got 404s on every fresh-clone download.
	if !strings.Contains(leylineReleaseURLTemplate, "ley-line-open") {
		t.Errorf("leylineReleaseURLTemplate must point at ley-line-open (the public repo), got %q", leylineReleaseURLTemplate)
	}
	if strings.Contains(leylineReleaseURLTemplate, "/ley-line/") {
		t.Errorf("leylineReleaseURLTemplate still references the private ley-line repo: %q", leylineReleaseURLTemplate)
	}
}

// TestLeylineReleaseURL_IncludesVersion pins the second half of the
// mache-9051f0 fix: the auto-download URL must be version-pinned, not
// `/releases/latest/download/`. Floating-latest is a recipe for picking
// up a daemon binary that doesn't match mache's schema-client pin in
// go.mod, which produces wire-format decode errors that are very hard
// to diagnose from the consumer side. The constructed URL must:
//   - reference the pinned version constant leylineBinaryVersion, and
//   - NOT contain the string "/releases/latest/" — every download
//     resolves to an exact tag.
func TestLeylineReleaseURL_IncludesVersion(t *testing.T) {
	// Pinned version must be a non-empty `vX.Y.Z`-shaped string.
	require.NotEmpty(t, leylineBinaryVersion, "leylineBinaryVersion must not be empty")
	assert.True(t, strings.HasPrefix(leylineBinaryVersion, "v"),
		"leylineBinaryVersion must be a vX.Y.Z tag (got %q)", leylineBinaryVersion)

	// The template substitutes (version, asset). Render with a stand-in
	// asset and assert the rendered URL embeds the pinned version and
	// does not fall back to /releases/latest/.
	rendered := fmt.Sprintf(leylineReleaseURLTemplate, leylineBinaryVersion, "leyline-linux-amd64")
	assert.Contains(t, rendered, leylineBinaryVersion,
		"rendered download URL must include the pinned version (got %q)", rendered)
	assert.NotContains(t, rendered, "/releases/latest/",
		"rendered download URL must NOT use /releases/latest/ — floating-latest defeats the version pin (got %q)", rendered)
	assert.Contains(t, rendered, "ley-line-open/releases/download/",
		"rendered download URL must use the /releases/download/<tag>/ form (got %q)", rendered)
}

// TestSendOp_ConcurrentCallsDoNotInterleave is the regression guard
// for Copilot's PR #383 review: SocketClient.SendOp performs an
// unsynchronized write-then-read on a shared connection. Without a
// mutex, concurrent callers (e.g. the file watcher firing multiple
// debounce timers in quick succession after a save burst) would
// interleave requests and reads, corrupting the line-delimited
// JSON protocol. Pinned here so a future refactor can't drop the
// serialization without the race detector catching it.
//
// The mock server tags each response with the request's "id" field;
// after N parallel SendOps, every caller must see a response whose
// id matches its own request. Without the mutex, ids will mismatch
// (or the read will EOF) under -race.
func TestSendOp_ConcurrentCallsDoNotInterleave(t *testing.T) {
	const calls = 50

	sockPath := mockServer(t, func(req map[string]any) map[string]any {
		// Echo the id so callers can verify their request matched
		// the response they read.
		return map[string]any{"id": req["id"], "ok": true}
	})

	sock, err := DialSocket(sockPath)
	require.NoError(t, err)
	defer sock.Close() //nolint:errcheck

	var (
		wg         sync.WaitGroup
		mismatches int64
		errs       int64
	)
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			resp, err := sock.SendOp(map[string]any{"op": "echo", "id": float64(id)})
			if err != nil {
				atomic.AddInt64(&errs, 1)
				return
			}
			gotID, _ := resp["id"].(float64)
			if int(gotID) != id {
				atomic.AddInt64(&mismatches, 1)
			}
		}(i)
	}
	wg.Wait()

	assert.Zero(t, atomic.LoadInt64(&errs), "no SendOp call should error under concurrent load")
	assert.Zero(t, atomic.LoadInt64(&mismatches),
		"every caller must read the response matching its own request id — mismatches mean the line-delimited protocol got crossed")
}

// captureLog redirects the default logger to a thread-safe buffer for the
// duration of the test and returns a snapshot accessor. Used by the
// Subscribe observability tests to assert that the goroutine logged the
// expected message on each silent-failure path.
func captureLog(t *testing.T) func() string {
	t.Helper()
	var (
		mu  sync.Mutex
		buf bytes.Buffer
	)
	// Wrap the buffer so writes are serialized — log.Output is called
	// from the Subscribe goroutine and the test goroutine reads it.
	prevOut := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&lockedWriter{mu: &mu, buf: &buf})
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

type lockedWriter struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// subscribePushServer accepts a single subscribe op, sends the supplied
// response line, and then yields the raw connection over the returned
// channel so the test can push arbitrary lines (valid events, malformed
// JSON, or nothing at all) into the SocketClient's read side.
//
// Use this instead of mockServer when the test needs to drive the
// Subscribe goroutine directly rather than the request/response path.
func subscribePushServer(t *testing.T, subscribeResp map[string]any) (sockPath string, connCh chan net.Conn) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "leyline-sub-test-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockPath = filepath.Join(dir, "t.sock")

	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	connCh = make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		rd := bufio.NewReader(conn)
		// Read the subscribe request, then send the canned response.
		if _, err := rd.ReadString('\n'); err != nil {
			_ = conn.Close()
			return
		}
		data, _ := json.Marshal(subscribeResp)
		if _, err := conn.Write(append(data, '\n')); err != nil {
			_ = conn.Close()
			return
		}
		connCh <- conn
	}()
	return sockPath, connCh
}

// TestSubscribe_MalformedEventLogsAndContinues pins the malformed-event
// silent-drop fix: prior code did `if err := json.Unmarshal(...); err != nil { continue }`
// with no operator signal. After the fix, each malformed line is logged
// and the goroutine keeps processing — a subsequent well-formed event
// must still reach the consumer's channel.
func TestSubscribe_MalformedEventLogsAndContinues(t *testing.T) {
	logSnap := captureLog(t)
	sockPath, connCh := subscribePushServer(t, map[string]any{"ok": true})

	client, err := DialSocket(sockPath)
	require.NoError(t, err)
	defer client.Close() //nolint:errcheck

	ch, err := client.Subscribe([]string{"x"})
	require.NoError(t, err)

	conn := <-connCh
	defer conn.Close() //nolint:errcheck

	// Garbage line, then a well-formed event.
	_, err = conn.Write([]byte("{this-is-not-json\n"))
	require.NoError(t, err)
	good := map[string]any{"event": true, "topic": "x", "payload": "ok"}
	data, _ := json.Marshal(good)
	_, err = conn.Write(append(data, '\n'))
	require.NoError(t, err)

	select {
	case ev := <-ch:
		assert.Equal(t, "x", ev["topic"], "well-formed event after malformed line must reach the consumer")
	case <-time.After(2 * time.Second):
		t.Fatal("expected event after malformed line within 2s")
	}

	assert.Contains(t, logSnap(), "subscribe: drop malformed event",
		"malformed event must be logged, not silently dropped")
}

// TestSubscribe_ReadDeadlineLogged pins the deadline-vs-EOF disambiguation
// fix: before, a 60s deadline timeout closed the channel identically to a
// clean EOF, so operators triaging "stale cache" reports couldn't tell
// "daemon SIGKILLed" from "daemon closed cleanly". After the fix, the
// timeout path emits a distinct log line.
//
// To avoid actually waiting 60s, we drive runSubscribeLoop directly with
// a 50ms timeout and a server that accepts the subscribe but never pushes
// any event lines.
func TestSubscribe_ReadDeadlineLogged(t *testing.T) {
	logSnap := captureLog(t)
	sockPath, connCh := subscribePushServer(t, map[string]any{"ok": true})

	client, err := DialSocket(sockPath)
	require.NoError(t, err)
	defer client.Close() //nolint:errcheck

	// Drive the subscribe handshake manually so we can call
	// runSubscribeLoop with a sub-second timeout. The production
	// Subscribe() always uses defaultSubscribeReadTimeout (60s),
	// which would make this test wall-clock-slow without the seam.
	_, err = client.conn.Write([]byte(`{"op":"subscribe","topics":["x"]}` + "\n"))
	require.NoError(t, err)
	_, err = client.rd.ReadString('\n') // consume "ok" handshake line
	require.NoError(t, err)

	conn := <-connCh
	// Server holds the conn but never pushes — the goroutine's read
	// deadline will fire first.
	defer conn.Close() //nolint:errcheck

	ch := make(chan map[string]any, 4)
	done := make(chan struct{})
	go func() {
		client.runSubscribeLoop(ch, 50*time.Millisecond)
		close(done)
	}()

	select {
	case <-done:
		// Channel must be closed too (drained → zero-value receive).
		_, open := <-ch
		assert.False(t, open, "channel must be closed after deadline timeout")
	case <-time.After(2 * time.Second):
		t.Fatal("Subscribe goroutine did not return after read deadline")
	}

	assert.Contains(t, logSnap(), "subscribe: read deadline exceeded",
		"deadline timeout must log distinctly from clean EOF so operators can tell SIGKILL from clean shutdown")
}

// TestSubscribe_SlowConsumerCountsAndLogsDrops pins the slow-consumer
// silent-drop fix: prior code did `select { case ch <- ev: default: /* drop */ }`
// with no counter and no log. After the fix:
//   - Every dropped event increments SubscribeDropped()
//   - The first drop emits a log line so operators see the signal
//     before stale-cache decisions pile up
//
// We fill the buffered channel (64) plus extras and never read; the
// goroutine must drop the overflow and SubscribeDropped() must advance.
func TestSubscribe_SlowConsumerCountsAndLogsDrops(t *testing.T) {
	logSnap := captureLog(t)
	sockPath, connCh := subscribePushServer(t, map[string]any{"ok": true})

	client, err := DialSocket(sockPath)
	require.NoError(t, err)
	defer client.Close() //nolint:errcheck

	ch, err := client.Subscribe([]string{"x"})
	require.NoError(t, err)
	_ = ch // intentionally never read — drives the drop path

	conn := <-connCh
	defer conn.Close() //nolint:errcheck

	// Push more events than the channel buffer (64). The goroutine
	// will fill the buffer, then start dropping on every subsequent
	// event. We push 80 → expect at least ~16 drops.
	const totalPushed = 80
	good := map[string]any{"event": true, "topic": "x"}
	data, _ := json.Marshal(good)
	line := append(data, '\n')
	for i := 0; i < totalPushed; i++ {
		if _, err := conn.Write(line); err != nil {
			t.Fatalf("push event %d: %v", i, err)
		}
	}

	// SubscribeDropped is updated async by the goroutine — poll briefly
	// rather than sleeping a fixed amount.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if client.SubscribeDropped() > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	dropped := client.SubscribeDropped()
	assert.Positive(t, dropped, "consumer never read; SubscribeDropped() must be non-zero")
	assert.Contains(t, logSnap(), "subscribe: dropping events (consumer behind)",
		"first drop must emit a log line so operators see the signal before stale-cache decisions pile up")
}

// swapLeylineReleaseURLTemplate replaces leylineReleaseURLTemplate for the
// duration of a test, restoring the original via t.Cleanup. This is the
// hermetic-test seam for downloadLeyline — production code never mutates
// the template, only tests do, so the global swap is safe as long as tests
// in this file don't run downloadLeyline in parallel (they don't).
func swapLeylineReleaseURLTemplate(t *testing.T, replacement string) {
	t.Helper()
	orig := leylineReleaseURLTemplate
	leylineReleaseURLTemplate = replacement
	t.Cleanup(func() { leylineReleaseURLTemplate = orig })
}

// TestDownloadLeyline_HappyPath exercises the 200-OK branch of
// downloadLeyline without touching the network. It stands up an
// httptest.Server that returns a known payload, swaps the release-URL
// template to point at that server, then asserts the binary lands on
// disk with the right bytes and an executable bit set. This covers the
// assetName / URL construction lines plus the io.Copy + chmod + rename
// path. Coverage-gate flagged the assetName line (L583) as new-prod
// uncovered in PR #388; this test closes that gap hermetically.
func TestDownloadLeyline_HappyPath(t *testing.T) {
	const wantBody = "fake-leyline-binary-payload"

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(wantBody))
	}))
	t.Cleanup(srv.Close)

	// Template still takes (version, asset) — keep the same %s/%s shape
	// so the assetName Sprintf inside downloadLeyline is still exercised.
	swapLeylineReleaseURLTemplate(t, srv.URL+"/releases/download/%s/%s")

	destDir := t.TempDir()
	destPath := filepath.Join(destDir, "leyline")

	out, err := downloadLeyline(destPath)
	require.NoError(t, err, "downloadLeyline must succeed on 200 OK")
	require.Equal(t, destPath, out, "downloadLeyline must return the requested dest path")

	// Asset name is built from runtime.GOOS/GOARCH inside downloadLeyline —
	// the server saw a URL of the form /releases/download/<version>/leyline-<os>-<arch>.
	// Don't pin os/arch (test runs on darwin AND linux CI); just assert the
	// /releases/download/<version>/leyline- prefix lines up.
	assert.Contains(t, gotPath, "/releases/download/"+leylineBinaryVersion+"/leyline-",
		"server should have received a version-pinned, asset-name-suffixed request path (got %q)", gotPath)

	// Verify file landed with the right bytes.
	gotBytes, err := os.ReadFile(destPath)
	require.NoError(t, err, "downloaded binary must exist on disk")
	assert.Equal(t, wantBody, string(gotBytes), "downloaded bytes must match server payload")

	// Verify the executable bit is set (chmod 0o755).
	info, err := os.Stat(destPath)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&0o111, "downloaded binary must have at least one executable bit set (got mode %v)", info.Mode())
}

// TestDownloadLeyline_NotFound exercises the 404 branch of downloadLeyline
// without touching the network. Coverage-gate flagged the StatusNotFound
// arm (L605) as new-prod uncovered in PR #388. The user-facing error must
// distinguish "this pinned version isn't published" from a generic network
// failure so operators can fix the pin (or fall back) instead of chasing
// a transport-layer ghost.
func TestDownloadLeyline_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	swapLeylineReleaseURLTemplate(t, srv.URL+"/releases/download/%s/%s")

	destDir := t.TempDir()
	destPath := filepath.Join(destDir, "leyline")

	_, err := downloadLeyline(destPath)
	require.Error(t, err, "downloadLeyline must return an error on HTTP 404")
	assert.Contains(t, err.Error(), "no leyline release available",
		"404 error must use the 'no release available' phrasing so operators can distinguish missing-pin from transport failure (got %q)", err.Error())
	assert.Contains(t, err.Error(), leylineBinaryVersion,
		"404 error must name the pinned version so operators know which tag to publish/repin (got %q)", err.Error())

	// Negative assertion: file must NOT have been created when download
	// failed — otherwise a stale zero-byte binary lingers and confuses
	// subsequent runs.
	_, statErr := os.Stat(destPath)
	assert.True(t, os.IsNotExist(statErr),
		"destPath must not exist after a failed download (got stat err %v)", statErr)
}

// resetManaged clears the package-level managed singleton between tests
// that exercise DiscoverOrStart. Without this, a prior test that primed
// managed.sock would let the next test's "no daemon found" assertion
// fail spuriously because the second-fast-path keeps the stale value.
//
// Takes *testing.T (used only for t.Helper); has no return value. The
// helper just zeros the singleton and kills any managed.proc — it does
// NOT register a t.Cleanup itself. Callers that want post-test cleanup
// must wrap it explicitly, e.g. t.Cleanup(func() { resetManaged(t) }).
func resetManaged(t *testing.T) {
	t.Helper()
	managed.mu.Lock()
	if managed.proc != nil {
		_ = managed.proc.Kill()
	}
	managed.proc = nil
	managed.sock = ""
	managed.mu.Unlock()
}

// TestDiscoverOrStart_OrphanedSocketRemovedThenSpawnsFresh pins the
// mache-52a23a dedup fix's stale-socket recovery path:
//
//   - A SIGKILL'd daemon leaves the well-known socket file behind with
//     no listener. The prior DiscoverOrStart did `os.Stat` and returned
//     that path, so the caller hit "connect: connection refused" long
//     after DiscoverOrStart claimed success.
//   - After the fix, the liveness probe (a real Dial) classifies the
//     file as stale, removes it, and proceeds to the spawn path. We
//     can't actually spawn leyline in unit tests, so MACHE_NO_LEYLINE=1
//     short-circuits the spawn with a distinctive error. The observable
//     contract is: (a) the stale socket file is GONE after the call,
//     proving the cleanup ran; (b) the error is the documented "no
//     binary" sentinel, proving execution reached the spawn block.
func TestDiscoverOrStart_OrphanedSocketRemovedThenSpawnsFresh(t *testing.T) {
	resetManaged(t)
	t.Cleanup(func() { resetManaged(t) })

	home := t.TempDir()
	dataDir := filepath.Join(home, ".mache")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	sockPath := filepath.Join(dataDir, "default.sock")

	// Plant an orphaned socket file — a touch-file, no listener.
	require.NoError(t, os.WriteFile(sockPath, []byte{}, 0o600))

	t.Setenv("HOME", home)
	t.Setenv("LEYLINE_SOCKET", "") // force well-known path
	t.Setenv("PATH", "/nonexistent-path-for-test")
	t.Setenv("MACHE_NO_LEYLINE", "1")

	_, err := DiscoverOrStart()
	require.Error(t, err, "spawn must be reached (no binary → MACHE_NO_LEYLINE error)")
	assert.Contains(t, err.Error(), "MACHE_NO_LEYLINE",
		"liveness probe must classify the touch-file as stale and proceed to spawn")

	_, statErr := os.Stat(sockPath)
	assert.True(t, os.IsNotExist(statErr),
		"stale socket file must be removed before spawn so the daemon can re-bind; got stat err=%v", statErr)
}

// TestDiscoverOrStart_LiveDaemonNotRespawned pins the "don't spawn over
// a healthy listener" path: if findExistingSocket returns a path AND a
// Dial connects, DiscoverOrStart must return that path immediately. No
// process is spawned, the env-gate (MACHE_NO_LEYLINE=1) is never hit.
func TestDiscoverOrStart_LiveDaemonNotRespawned(t *testing.T) {
	resetManaged(t)
	t.Cleanup(func() { resetManaged(t) })

	dir, err := os.MkdirTemp("/tmp", "leyline-live-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "live.sock")

	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go acceptAndClose(ln)

	t.Setenv("LEYLINE_SOCKET", sockPath)
	// Belt-and-braces: even if the liveness check were broken, the spawn
	// branch would error with MACHE_NO_LEYLINE — which would surface as
	// the test error and tell us the dedup gate let us through.
	t.Setenv("PATH", "/nonexistent-path-for-test")
	t.Setenv("MACHE_NO_LEYLINE", "1")

	found, err := DiscoverOrStart()
	require.NoError(t, err, "live daemon must short-circuit before the spawn block")
	assert.Equal(t, sockPath, found)
}

// TestDiscoverOrStart_ConcurrentCallersDoNotDoubleSpawn pins the
// in-process dedup property at the heart of mache-52a23a's "6 orphan
// daemons" observation: even when many callers invoke DiscoverOrStart
// in parallel, exactly one daemon's socket is reported, and the spawn
// path is entered at most once.
//
// We can't spawn real leyline in a unit test, so we stand up a real UDS
// listener at the well-known path BEFORE the calls and confirm every
// caller reuses it. If the lock-then-probe sequence were missing,
// goroutine A could spawn while goroutine B was still inside the
// pre-lock fast path — observable here as one caller's path differing,
// or any caller hitting the MACHE_NO_LEYLINE spawn-fail error.
func TestDiscoverOrStart_ConcurrentCallersDoNotDoubleSpawn(t *testing.T) {
	resetManaged(t)
	t.Cleanup(func() { resetManaged(t) })

	dir, err := os.MkdirTemp("/tmp", "leyline-concurrent-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "shared.sock")

	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go acceptAndClose(ln)

	t.Setenv("LEYLINE_SOCKET", sockPath)
	t.Setenv("PATH", "/nonexistent-path-for-test")
	t.Setenv("MACHE_NO_LEYLINE", "1")

	const goroutines = 16
	var (
		wg      sync.WaitGroup
		results = make([]string, goroutines)
		errs    = make([]error, goroutines)
		start   = make(chan struct{})
	)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start // line everyone up so the race window is real
			path, err := DiscoverOrStart()
			results[idx] = path
			errs[idx] = err
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		require.NoErrorf(t, err, "goroutine %d failed — concurrent dedup let one caller fall through to the spawn block", i)
		assert.Equalf(t, sockPath, results[i],
			"goroutine %d returned %q; all callers must reuse the single live socket", i, results[i])
	}
}

// e2eHome builds a HOME dir for DiscoverOrStart's spawn-path tests. The
// daemon's UDS path is derived from <HOME>/.mache/default.sock and macOS
// caps sun_path at ~104 bytes — Go's t.TempDir lives under
// /var/folders/.../TestName/NNN/ which blows that budget every time. Put
// HOME under /tmp/<short> and clean it explicitly since it lives outside
// t.TempDir's reach. Same constraint observed in sheaf_e2e_test.go.
func e2eHome(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "leyline-disc-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// TestDiscoverOrStart_LocalBinFallback_SocketTimeout covers two production
// branches that the on-PATH happy path skips:
//
//   - The `~/.mache/bin/leyline` fallback (lines 218-222): leyline not on
//     PATH but a cached binary exists in the local bin dir.
//   - The "spawned but socket never appeared within 5s" timeout (lines
//     300-303): exec.Start succeeds but the binary doesn't bind the
//     daemon UDS, so the poll loop must time out, kill the process, and
//     surface a distinctive error.
//
// We satisfy both with /bin/sh as the fake binary — it ignores the
// daemon flags, exits immediately, and never binds a socket. The poll
// loop runs the full 5s before declaring failure, so this test costs
// ~5s; that's the price of covering the timeout path without a more
// elaborate harness binary.
func TestDiscoverOrStart_LocalBinFallback_SocketTimeout(t *testing.T) {
	resetManaged(t)
	t.Cleanup(func() { resetManaged(t) })

	home := e2eHome(t)
	binDir := filepath.Join(home, ".mache", "bin")
	require.NoError(t, os.MkdirAll(binDir, 0o755))
	// Symlink /bin/sh into ~/.mache/bin/leyline. exec.LookPath against
	// PATH must fail (sentinel path), then os.Stat on the local-bin
	// fallback succeeds, so DiscoverOrStart picks up the fake binary
	// from the cache location and proceeds to cmd.Start.
	require.NoError(t, os.Symlink("/bin/sh", filepath.Join(binDir, "leyline")))

	t.Setenv("HOME", home)
	t.Setenv("LEYLINE_SOCKET", "")
	t.Setenv("PATH", "/nonexistent-path-for-test")
	t.Setenv("MACHE_NO_LEYLINE", "")

	_, err := DiscoverOrStart()
	require.Error(t, err, "fake binary that doesn't bind the socket must time out")
	assert.Contains(t, err.Error(), "did not appear within 5s",
		"timeout path must surface the documented 'socket did not appear' error")

	// Singleton must be reset by the timeout cleanup (lines 301-302) so
	// a follow-up DiscoverOrStart can retry without inheriting the
	// dead fake process.
	managed.mu.Lock()
	leftoverProc := managed.proc
	leftoverSock := managed.sock
	managed.mu.Unlock()
	assert.Nil(t, leftoverProc, "timeout cleanup must clear managed.proc")
	assert.Empty(t, leftoverSock, "timeout cleanup must clear managed.sock")
}

// TestDiscoverOrStart_ManagedFastPathReturnsLiveSocket pins fast-path 2
// (the `managed.sock != "" && isSocketAlive(managed.sock)` happy branch
// at lines 197-200). Normally a second in-process call short-circuits
// via fast-path 1 (findExistingSocket), but in deployments where HOME
// or LEYLINE_SOCKET don't resolve to a socket file the in-memory
// managed singleton is the only fast path left.
//
// Setup: prime managed with a real UDS listener, then call
// DiscoverOrStart with HOME pointed at a directory that does NOT
// contain default.sock and LEYLINE_SOCKET cleared, forcing
// findExistingSocket to error and execution to reach fast-path 2.
func TestDiscoverOrStart_ManagedFastPathReturnsLiveSocket(t *testing.T) {
	resetManaged(t)
	t.Cleanup(func() { resetManaged(t) })

	dir, err := os.MkdirTemp("/tmp", "leyline-fp2-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "managed.sock")

	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go acceptAndClose(ln)

	// Prime the managed singleton directly. We don't have a real
	// os.Process here, and DiscoverOrStart's fast-path 2 only reads
	// managed.sock — managed.proc is only inspected in the stale
	// branch (which we're NOT exercising) so leaving it nil is fine.
	managed.mu.Lock()
	managed.sock = sockPath
	managed.proc = nil
	managed.mu.Unlock()

	// HOME points at an empty dir → no <HOME>/.mache/default.sock →
	// findExistingSocket returns "no socket found" → execution falls
	// through to fast-path 2.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LEYLINE_SOCKET", "")
	// If fast-path 2 misfired, the spawn block would error on the
	// missing binary — this gate makes that the unmistakable signal.
	t.Setenv("PATH", "/nonexistent-path-for-test")
	t.Setenv("MACHE_NO_LEYLINE", "1")

	found, err := DiscoverOrStart()
	require.NoError(t, err, "managed.sock fast-path must short-circuit before the spawn block")
	assert.Equal(t, sockPath, found, "fast-path 2 must return the primed managed.sock")
}

// TestDiscoverOrStart_ExplicitSocketStaleErrors pins the "user-supplied
// LEYLINE_SOCKET points at a stale path" path. When the env var is set
// but no daemon is listening, mache must NOT silently remove the user's
// file (it isn't ours — only the well-known <HOME>/.mache/default.sock
// is mache-owned). Instead, DiscoverOrStart surfaces a distinctive
// "exists but no daemon is listening" error so the operator can fix
// their setup. Pins lines 189-191 of socket.go.
func TestDiscoverOrStart_ExplicitSocketStaleErrors(t *testing.T) {
	resetManaged(t)
	t.Cleanup(func() { resetManaged(t) })

	dir, err := os.MkdirTemp("/tmp", "leyline-stale-env-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "stale.sock")
	// Touch-file with no listener: findExistingSocket returns this path
	// via the env-var branch, then isSocketAlive must reject it, and
	// the env-set branch must error rather than removing the file.
	require.NoError(t, os.WriteFile(sockPath, []byte{}, 0o600))

	t.Setenv("LEYLINE_SOCKET", sockPath)
	// Belt-and-braces: short-circuit the spawn path if the env-stale
	// branch were skipped, so we'd see the wrong error.
	t.Setenv("MACHE_NO_LEYLINE", "1")

	_, err = DiscoverOrStart()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exists but no daemon is listening",
		"explicit LEYLINE_SOCKET pointing at a stale path must surface a distinct error, not silently remove the user's file")

	// File must still exist — mache must not touch sockets it doesn't own.
	_, statErr := os.Stat(sockPath)
	assert.NoError(t, statErr, "user-supplied LEYLINE_SOCKET file must NOT be removed by mache")
}

// TestDiscoverOrStart_E2E_HappyPath spawns a real leyline daemon via the
// production DiscoverOrStart path and asserts:
//
//   - the returned sockPath is non-empty and matches <HOME>/.mache/default.sock
//   - the socket file exists on disk after the call
//   - a fresh Dial succeeds, proving a real listener was bound (not just
//     a stale inode left over from a SIGKILL)
//
// This exercises the cmd.Start / wait-for-socket / managed-record block
// that the mocked tests intentionally skip via MACHE_NO_LEYLINE=1. Skips
// automatically when the leyline binary is not on PATH so CI workers
// without it stay green — same idiom as sheaf_e2e_test.go.
func TestDiscoverOrStart_E2E_HappyPath(t *testing.T) {
	if _, err := exec.LookPath("leyline"); err != nil {
		t.Skip("leyline binary not on PATH — skipping E2E DiscoverOrStart")
	}
	resetManaged(t)
	t.Cleanup(func() {
		StopManaged()
		resetManaged(t)
	})

	home := e2eHome(t)
	t.Setenv("HOME", home)
	// Clear LEYLINE_SOCKET so DiscoverOrStart falls through to the
	// well-known path and exercises the spawn branch rather than the
	// env-var fast path.
	t.Setenv("LEYLINE_SOCKET", "")
	// Ensure the env-gate doesn't short-circuit the spawn.
	t.Setenv("MACHE_NO_LEYLINE", "")

	sockPath, err := DiscoverOrStart()
	require.NoError(t, err, "DiscoverOrStart must spawn a live daemon when leyline is on PATH")
	require.NotEmpty(t, sockPath, "spawn must return a socket path")
	assert.Equal(t, filepath.Join(home, ".mache", "default.sock"), sockPath,
		"managed daemon must bind the well-known <HOME>/.mache/default.sock path")

	_, statErr := os.Stat(sockPath)
	require.NoError(t, statErr, "spawn must leave the socket file on disk")

	// Real Dial confirms a listener is alive, not just an inode. This is
	// the same liveness criterion isSocketAlive uses; reaching it through
	// DiscoverOrStart proves the post-spawn poll loop returned only after
	// the daemon's accept loop was bound.
	conn, dialErr := net.DialTimeout("unix", sockPath, 2*time.Second)
	require.NoError(t, dialErr, "spawned daemon must accept a Dial")
	_ = conn.Close()

	// managed singleton must reflect the spawned daemon so StopManaged
	// can reap it; if proc is nil, t.Cleanup's StopManaged would leak.
	managed.mu.Lock()
	hasProc := managed.proc != nil
	recordedSock := managed.sock
	managed.mu.Unlock()
	assert.True(t, hasProc, "managed.proc must be recorded after spawn so StopManaged can reap it")
	assert.Equal(t, sockPath, recordedSock, "managed.sock must match the returned path")
}

// TestDiscoverOrStart_E2E_ManagedReusedAcrossCalls asserts the fast path
// 2 (in-process managed reuse) covers a real daemon: after a first call
// spawns and records the managed singleton, a second call returns the
// same path WITHOUT spawning a new process.
//
// This pins the contract that callers in the same process don't pay
// repeated spawn cost — the heart of the mache-52a23a dedup work for
// process-internal callers. Sibling test
// TestDiscoverOrStart_ConcurrentCallersDoNotDoubleSpawn covers the
// concurrent variant against a mock listener; this test covers the
// sequential variant against a real daemon.
func TestDiscoverOrStart_E2E_ManagedReusedAcrossCalls(t *testing.T) {
	if _, err := exec.LookPath("leyline"); err != nil {
		t.Skip("leyline binary not on PATH — skipping E2E DiscoverOrStart")
	}
	resetManaged(t)
	t.Cleanup(func() {
		StopManaged()
		resetManaged(t)
	})

	home := e2eHome(t)
	t.Setenv("HOME", home)
	t.Setenv("LEYLINE_SOCKET", "")
	t.Setenv("MACHE_NO_LEYLINE", "")

	first, err := DiscoverOrStart()
	require.NoError(t, err, "first DiscoverOrStart must spawn a daemon")
	require.NotEmpty(t, first)

	managed.mu.Lock()
	firstPid := 0
	if managed.proc != nil {
		firstPid = managed.proc.Pid
	}
	managed.mu.Unlock()
	require.NotZero(t, firstPid, "first call must record a managed.proc with a pid")

	second, err := DiscoverOrStart()
	require.NoError(t, err, "second DiscoverOrStart must reuse the live daemon")
	assert.Equal(t, first, second, "second call must return the same socket path")

	managed.mu.Lock()
	secondPid := 0
	if managed.proc != nil {
		secondPid = managed.proc.Pid
	}
	managed.mu.Unlock()
	assert.Equal(t, firstPid, secondPid,
		"second call must NOT spawn a new process — managed.proc.Pid must be unchanged")
}

// TestDiscoverOrStart_E2E_DaemonCrashRecovery pins the stale-managed-
// daemon recovery path (the `managed.sock != "" && !isSocketAlive`
// branch inside DiscoverOrStart). Sequence:
//
//  1. DiscoverOrStart spawns a daemon and records managed.{proc,sock}.
//  2. We SIGKILL the daemon directly — the socket inode stays on disk
//     (kernel doesn't clean it up) but nothing listens. This is the
//     real "daemon crashed" shape, distinct from the cooperative
//     SIGTERM path StopManaged uses.
//  3. A second DiscoverOrStart must observe the dead listener, reap +
//     reset the singleton, remove the stale inode, and spawn fresh.
//     Observable: a new pid and a successful Dial post-call.
//
// This is the only test that hits lines 200-208 (stale-managed reap +
// reset) with a real daemon process; the existing
// OrphanedSocketRemovedThenSpawnsFresh test exercises the on-disk-only
// stale path with MACHE_NO_LEYLINE gating the spawn.
func TestDiscoverOrStart_E2E_DaemonCrashRecovery(t *testing.T) {
	if _, err := exec.LookPath("leyline"); err != nil {
		t.Skip("leyline binary not on PATH — skipping E2E DiscoverOrStart")
	}
	resetManaged(t)
	t.Cleanup(func() {
		StopManaged()
		resetManaged(t)
	})

	home := e2eHome(t)
	t.Setenv("HOME", home)
	t.Setenv("LEYLINE_SOCKET", "")
	t.Setenv("MACHE_NO_LEYLINE", "")

	sockPath, err := DiscoverOrStart()
	require.NoError(t, err)
	require.NotEmpty(t, sockPath)

	managed.mu.Lock()
	firstProc := managed.proc
	managed.mu.Unlock()
	require.NotNil(t, firstProc, "first spawn must record managed.proc")
	firstPid := firstProc.Pid

	// SIGKILL the daemon and wait for the kernel to reap it. We can't
	// just call StopManaged here — that resets managed.{proc,sock}, and
	// the whole point of this test is that DiscoverOrStart itself does
	// the recovery when it finds a recorded-but-dead managed daemon.
	require.NoError(t, firstProc.Signal(syscall.SIGKILL))
	_, _ = firstProc.Wait()

	// Wait until the socket is observably dead. SIGKILL drops the
	// listener immediately, but giving the kernel a brief beat avoids
	// any platform-specific connect-on-dying-socket flakiness.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !isSocketAlive(sockPath) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.False(t, isSocketAlive(sockPath),
		"after SIGKILL the socket must not accept connections — recovery path requires this")

	// Recovery call: must detect the dead managed daemon, reap +
	// reset, remove the stale inode, and spawn fresh.
	recovered, err := DiscoverOrStart()
	require.NoError(t, err, "DiscoverOrStart must recover from a crashed managed daemon")
	assert.Equal(t, sockPath, recovered, "recovered daemon must rebind the well-known path")

	managed.mu.Lock()
	secondProc := managed.proc
	managed.mu.Unlock()
	require.NotNil(t, secondProc, "recovery must record a new managed.proc")
	assert.NotEqual(t, firstPid, secondProc.Pid,
		"recovery must spawn a new process — same pid means the dead-daemon branch was skipped")

	// Sanity-check: the fresh daemon is actually accepting.
	conn, dialErr := net.DialTimeout("unix", recovered, 2*time.Second)
	require.NoError(t, dialErr, "recovered daemon must accept a Dial")
	_ = conn.Close()
}

// TestQuery_PropagatesSendOpError pins the early-return branch of Query
// (L470 in commit 3c73e6b): when SendOp fails — e.g. the daemon closes
// the connection mid-RPC — Query must surface that error directly rather
// than masking it as a nil rows result. We force the failure with a
// past-deadline SetDeadline so SendOp's Write (or follow-on Read) trips
// the i/o timeout, which SendOp wraps and returns to Query.
func TestQuery_PropagatesSendOpError(t *testing.T) {
	sockPath := mockServer(t, func(req map[string]any) map[string]any {
		return map[string]any{"ok": true}
	})

	client, err := DialSocket(sockPath)
	require.NoError(t, err)
	defer client.Close() //nolint:errcheck

	// Past deadline forces the next Read/Write to fail immediately with
	// an i/o timeout. Using SetDeadline rather than Close avoids the nil
	// c.conn deref that Close triggers; the goal here is to drive SendOp
	// into its error return so Query's `if err != nil { return nil, err }`
	// branch executes.
	require.NoError(t, client.SetDeadline(time.Unix(0, 1)))

	_, err = client.Query("SELECT 1")
	require.Error(t, err, "Query must surface SendOp errors instead of returning (nil, nil)")
}

// TestQuery_UnexpectedRowsType pins the rows type-assertion failure
// branch (L482-483 in commit 3c73e6b): when the daemon returns a `rows`
// field whose JSON shape isn't `[]any` (e.g. a bare string from a
// version-skewed daemon, or a future shape we haven't taught Query to
// parse), Query must reject the response with a typed error rather than
// panic on the assertion or silently return empty rows.
func TestQuery_UnexpectedRowsType(t *testing.T) {
	sockPath := mockServer(t, func(req map[string]any) map[string]any {
		// Return a string where Query expects []any. JSON-decode of the
		// response gives us resp["rows"] of dynamic type string, which
		// fails the `rows.([]any)` assertion.
		return map[string]any{"rows": "not-an-array"}
	})

	client, err := DialSocket(sockPath)
	require.NoError(t, err)
	defer client.Close() //nolint:errcheck

	_, err = client.Query("SELECT 1")
	require.Error(t, err, "Query must reject a non-[]any rows field")
	assert.Contains(t, err.Error(), "unexpected rows type",
		"error must name the surprise type so operators can fingerprint the daemon-version mismatch (got %q)", err.Error())
}

// TestSubscribe_WriteFailsOnExpiredDeadline pins the write-failure branch
// (L523-524 in commit 3c73e6b): if the UDS connection isn't writable
// when Subscribe issues its handshake, the error must propagate with the
// "write subscribe" wrap rather than getting hidden under a later
// read-side failure. We force the Write to fail by setting a past
// deadline before Subscribe so the very first c.conn.Write inside
// Subscribe returns an i/o-timeout error.
func TestSubscribe_WriteFailsOnExpiredDeadline(t *testing.T) {
	sockPath := mockServer(t, func(req map[string]any) map[string]any {
		return map[string]any{"ok": true}
	})

	client, err := DialSocket(sockPath)
	require.NoError(t, err)
	defer client.Close() //nolint:errcheck

	// Past deadline → next Write trips i/o timeout. SetDeadline (rather
	// than Close) preserves c.conn so we drive Subscribe's c.conn.Write
	// to its error path without nil-deref on the test side.
	require.NoError(t, client.SetDeadline(time.Unix(0, 1)))

	_, err = client.Subscribe([]string{"x"})
	require.Error(t, err, "Subscribe must surface the write failure")
	assert.Contains(t, err.Error(), "write subscribe",
		"error must wrap with 'write subscribe' so the failing leg is identifiable in logs (got %q)", err.Error())
}

// TestSubscribe_ReadResponseFailsOnEarlyClose pins the
// read-subscribe-response branch (L529-530 in commit 3c73e6b): the
// daemon accepted and read our subscribe op but disconnected before
// sending an ack. Subscribe must surface the read failure with the
// documented "read subscribe response" wrap, NOT a misleading
// "unmarshal subscribe response" (which would suggest the daemon
// returned garbage rather than nothing).
func TestSubscribe_ReadResponseFailsOnEarlyClose(t *testing.T) {
	// Build a mock that accepts the subscribe write, then closes the
	// connection without responding. We don't use subscribePushServer
	// because that one always sends a canned response line.
	dir, err := os.MkdirTemp("/tmp", "leyline-sub-noreply-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "t.sock")

	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		rd := bufio.NewReader(conn)
		// Consume the subscribe request line so the client's Write
		// completes successfully; then close without replying.
		_, _ = rd.ReadString('\n')
		_ = conn.Close()
	}()

	client, err := DialSocket(sockPath)
	require.NoError(t, err)
	defer client.Close() //nolint:errcheck

	_, err = client.Subscribe([]string{"x"})
	require.Error(t, err, "Subscribe must surface the read failure when the daemon disconnects without acking")
	assert.Contains(t, err.Error(), "read subscribe response",
		"error must wrap with 'read subscribe response' so the failing leg is identifiable (got %q)", err.Error())
}

// TestSubscribe_UnmarshalResponseFailsOnNonJSON pins the
// unmarshal-response branch (L534 in commit 3c73e6b): the daemon
// replied to subscribe, but with bytes that don't parse as JSON.
// Subscribe must surface that as "unmarshal subscribe response" so
// operators can tell "daemon talking the wrong protocol" apart from
// "daemon disconnected" (L529-530's path) and "daemon returned a
// JSON-shaped error" (the L536-537 errMsg path).
func TestSubscribe_UnmarshalResponseFailsOnNonJSON(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "leyline-sub-badjson-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "t.sock")

	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close() //nolint:errcheck
		rd := bufio.NewReader(conn)
		if _, err := rd.ReadString('\n'); err != nil {
			return
		}
		// Send a line that isn't JSON.
		_, _ = conn.Write([]byte("not-json\n"))
	}()

	client, err := DialSocket(sockPath)
	require.NoError(t, err)
	defer client.Close() //nolint:errcheck

	_, err = client.Subscribe([]string{"x"})
	require.Error(t, err, "Subscribe must reject a non-JSON reply")
	assert.Contains(t, err.Error(), "unmarshal subscribe response",
		"error must wrap with 'unmarshal subscribe response' so a daemon-protocol mismatch isn't confused with a transport failure (got %q)", err.Error())
}
