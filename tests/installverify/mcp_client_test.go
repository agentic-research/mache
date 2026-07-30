package installverify

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A minimal MCP Streamable-HTTP client — deliberately hand-rolled.
//
// The server speaks github.com/mark3labs/mcp-go. Using that library's client
// here would verify the library against itself; the question this gate asks is
// whether an INSTALLED mache answers a third-party consumer on the wire, so it
// speaks the raw protocol instead. It is ~100 lines because Streamable HTTP is
// small: POST JSON-RPC, read the session id out of the Mcp-Session-Id response
// header, echo it on every subsequent call.
//
// One behaviour worth writing down, because it costs an unexplained 5s stall
// and then a confusing error otherwise: a client that does not implement the
// MCP ROOTS protocol cannot answer the server's roots/list request, so
// root-dependent tools time out and return "workspace root unavailable". This
// client does not implement roots — it does not need to, because the harness
// starts mache with an explicit `--path`, which makes the server resolve the
// session against that directory instead of asking.

const (
	// mcpProtocolVersion is the revision this client negotiates. Pinned, not
	// floating: a gate that silently renegotiates is a gate that can stop
	// testing the thing it was written for.
	mcpProtocolVersion = "2025-06-18"

	sessionHeader = "Mcp-Session-Id"
)

type mcpClient struct {
	url     string
	session string
	http    *http.Client
	nextID  int
}

// rpcResponse is the subset of a JSON-RPC response this gate reads.
type rpcResponse struct {
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Result json.RawMessage `json:"result"`
}

// toolResult is the subset of an MCP CallToolResult this gate reads. Every
// mache tool answers with a single text content block whose body is JSON.
type toolResult struct {
	IsError bool `json:"isError"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func newMCPClient(url string) *mcpClient {
	return &mcpClient{url: url, http: &http.Client{Timeout: 60 * time.Second}}
}

// post sends one JSON-RPC message and returns the decoded response. A nil
// response means the server accepted a notification with no body.
func (c *mcpClient) post(ctx context.Context, method string, params any) (*rpcResponse, http.Header, error) {
	c.nextID++
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	if !strings.HasPrefix(method, "notifications/") {
		msg["id"] = c.nextID
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.session != "" {
		req.Header.Set(sessionHeader, c.session)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, resp.Header, fmt.Errorf("%s: HTTP %d: %s", method, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	payload := unwrapSSE(resp.Header.Get("Content-Type"), raw)
	if len(bytes.TrimSpace(payload)) == 0 {
		return nil, resp.Header, nil // accepted notification
	}
	var out rpcResponse
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, resp.Header, fmt.Errorf("%s: decode response %q: %w", method, string(payload), err)
	}
	if out.Error != nil {
		return nil, resp.Header, fmt.Errorf("%s: JSON-RPC error %d: %s", method, out.Error.Code, out.Error.Message)
	}
	return &out, resp.Header, nil
}

// unwrapSSE extracts the JSON payload from an event-stream body. Streamable
// HTTP may answer either as application/json or as a one-event SSE frame; both
// carry the same JSON-RPC message.
func unwrapSSE(contentType string, raw []byte) []byte {
	if !strings.Contains(contentType, "text/event-stream") {
		return raw
	}
	var buf bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		if line, ok := strings.CutPrefix(sc.Text(), "data:"); ok {
			buf.WriteString(strings.TrimSpace(line))
		}
	}
	return buf.Bytes()
}

// initialize performs the MCP handshake and captures the session id, which
// every later call must echo — without it the server answers 404 "Invalid
// session ID".
func (c *mcpClient) initialize(ctx context.Context) error {
	_, hdr, err := c.post(ctx, "initialize", map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "mache-install-verify", "version": "1"},
	})
	if err != nil {
		return err
	}
	c.session = hdr.Get(sessionHeader)
	if c.session == "" {
		return fmt.Errorf("server returned no %s header; stateful sessions require one", sessionHeader)
	}
	_, _, err = c.post(ctx, "notifications/initialized", map[string]any{})
	return err
}

// callTool invokes an MCP tool and returns the text body of its single content
// block. A tool that answered with isError returns an error carrying that text,
// so "the tool ran" and "the tool worked" stay distinguishable.
func (c *mcpClient) callTool(ctx context.Context, name string, args map[string]any) (string, error) {
	if args == nil {
		args = map[string]any{}
	}
	resp, _, err := c.post(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return "", err
	}
	var tr toolResult
	if err := json.Unmarshal(resp.Result, &tr); err != nil {
		return "", fmt.Errorf("%s: decode tool result: %w", name, err)
	}
	if len(tr.Content) == 0 {
		return "", fmt.Errorf("%s: tool returned no content blocks", name)
	}
	text := tr.Content[0].Text
	if tr.IsError {
		return text, fmt.Errorf("%s: tool reported an error: %s", name, text)
	}
	return text, nil
}

// freePort reserves a port by binding and immediately releasing it. `mache
// serve` rejects a bare ":0" and never reports the port it actually bound, so
// the caller has to choose one. The window between release and bind is a
// theoretical race, not a practical one on a test host.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}

// startServe launches `mache serve <db> --http localhost:<port> --path <dir>`
// and blocks until the endpoint answers an initialize, returning a ready
// client. The process is killed and its output dumped on failure.
//
// --path is not optional here: without it the server asks the client for MCP
// roots, which this client does not implement, and every tool call fails with
// "workspace root unavailable" after a 5s stall.
func startServe(t *testing.T, bin, dbPath, basePath string) *mcpClient {
	t.Helper()
	port := freePort(t)
	url := fmt.Sprintf("http://localhost:%d/mcp", port)

	ctx, cancel := context.WithCancel(t.Context())
	c := exec.CommandContext(ctx, bin, "serve", dbPath,
		"--http", fmt.Sprintf("localhost:%d", port), "--path", basePath)

	// Child output goes to a FILE, not an in-process buffer. The readiness
	// loop reads it while the child is still writing, and a shared
	// bytes.Buffer would need its own lock; the kernel already provides one.
	logPath := filepath.Join(t.TempDir(), "serve.log")
	logFile, err := os.Create(logPath)
	require.NoError(t, err)
	c.Stdout, c.Stderr = logFile, logFile
	serveOutput := func() string {
		b, readErr := os.ReadFile(logPath)
		if readErr != nil {
			return fmt.Sprintf("(could not read %s: %v)", logPath, readErr)
		}
		return string(b)
	}
	require.NoError(t, c.Start(), "start mache serve")
	require.NoError(t, logFile.Close(), "the child holds its own descriptor")

	done := make(chan struct{})
	go func() { _ = c.Wait(); close(done) }()
	t.Cleanup(func() {
		// CommandContext kills the child on cancel, so Wait always returns and
		// this needs no timeout of its own.
		cancel()
		<-done
		if t.Failed() {
			t.Logf("mache serve output:\n%s", serveOutput())
		}
	})

	// The "listening on …" log line is printed BEFORE ListenAndServe, so it is
	// not proof the socket bound. Poll the endpoint instead — via
	// require.Eventually rather than a hand-rolled sleep loop, which is both
	// the repo's stated preference and what its sleep_in_test gate enforces.
	var (
		client  *mcpClient
		lastErr error
		exited  bool
	)
	require.Eventually(t, func() bool {
		select {
		case <-done:
			exited = true
			return true // stop polling; the assertions below explain why
		default:
		}
		candidate := newMCPClient(url)
		pingCtx, pingCancel := context.WithTimeout(ctx, 10*time.Second)
		defer pingCancel()
		if err := candidate.initialize(pingCtx); err != nil {
			lastErr = err
			return false
		}
		client = candidate
		return true
	}, 60*time.Second, 150*time.Millisecond,
		"mache serve never answered on %s (last error: %v)\n%s", url, lastErr, serveOutput())

	require.Falsef(t, exited, "mache serve exited before becoming ready:\n%s", serveOutput())
	require.NotNilf(t, client, "mache serve never completed an MCP handshake on %s: %v\n%s",
		url, lastErr, serveOutput())
	return client
}

// decodeJSON unmarshals a tool's text body, failing the test with the body
// quoted when it is not the JSON the assertion expects.
func decodeJSON[T any](t *testing.T, tool, body string) T {
	t.Helper()
	var v T
	require.NoErrorf(t, json.Unmarshal([]byte(body), &v),
		"%s returned a body that is not the expected JSON shape:\n%s", tool, body)
	return v
}

// requireFileExists fails when path is missing, quoting what was expected.
func requireFileExists(t *testing.T, path, what string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	require.NoErrorf(t, err, "%s missing at %s", what, path)
	return info
}
