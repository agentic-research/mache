package mcpserve

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mache's HTTP endpoint refuses a request body over 4 MiB, mcp-go's
// DefaultMaxRequestBodyBytes (v1.5.0; before it the read was unbounded, so any
// local client could make the daemon buffer an arbitrary body). mache keeps
// the default: write_file sends one node's content, far below it. Raising the
// bound is a decision, not a side effect of a dependency bump (mache-a1c114).
func TestHTTPServer_RefusesARequestBodyOver4MiB(t *testing.T) {
	ts := httptest.NewServer(newHTTPServer(server.NewMCPServer("t", "0")).Handler)
	t.Cleanup(ts.Close)
	post := func(body []byte) (int, string) {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	code, body := post([]byte(initialize))
	require.Equal(t, http.StatusOK, code, body)
	assert.Contains(t, body, `"serverInfo"`)

	// A well-formed request just past the bound, so the size is the only
	// reason to refuse it.
	const bound = 4 << 20
	pad := strings.Repeat("x", bound)
	big := `{"jsonrpc":"2.0","id":2,"method":"ping","params":{"pad":"` + pad + `"}}`
	code, body = post([]byte(big))
	assert.NotEqual(t, http.StatusOK, code, "a body over 4 MiB was accepted")
	assert.Contains(t, body, "exceeds")
}
