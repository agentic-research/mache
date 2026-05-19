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
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	f, _ := os.Create(sockPath)
	_ = f.Close()

	t.Setenv("LEYLINE_SOCKET", sockPath)
	found, err := DiscoverOrStart()
	if err != nil {
		t.Fatalf("DiscoverOrStart: %v", err)
	}
	if found != sockPath {
		t.Errorf("expected %s, got %s", sockPath, found)
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
