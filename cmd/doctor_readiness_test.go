package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/agentic-research/mache/internal/projcfg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeMCPDaemon serves an MCP handshake and hands tool calls to toolReply.
// It exists to build the state real probes could not see: a daemon that
// answers initialize perfectly and cannot serve.
func fakeMCPDaemon(t *testing.T, toolReply func(w http.ResponseWriter)) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Header.Get("Mcp-Session-Id") == "" {
			// initialize: always healthy, which is the whole point.
			w.Header().Set("Mcp-Session-Id", "mcp-session-fake")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"mache","version":"9.9.9"}}}`))
			return
		}
		toolReply(w)
	}))
	t.Cleanup(srv.Close)

	prev := projcfg.MacheHTTPURL
	projcfg.MacheHTTPURL = srv.URL
	t.Cleanup(func() { projcfg.MacheHTTPURL = prev })
}

// TestProbeDaemonReadiness_SeesWhatLivenessCannot is the point of the whole
// check (mache-956488): every probe mache had asked the MCP handshake, which
// the HTTP layer answers before any graph exists. A daemon whose graph build
// is wedged therefore reported as healthy while serving nothing.
func TestProbeDaemonReadiness_SeesWhatLivenessCannot(t *testing.T) {
	t.Run("serving", func(t *testing.T) {
		fakeMCPDaemon(t, func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"{}"}]}}`))
		})
		state, _ := probeDaemonReadiness(context.Background())
		assert.Equal(t, readyServing, state)
	})

	t.Run("building is distinguished from broken", func(t *testing.T) {
		fakeMCPDaemon(t, func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"Graph is still loading — the daemon is parsing source files."}]}}`))
		})
		state, detail := probeDaemonReadiness(context.Background())
		require.Equal(t, readyBuilding, state,
			"a building graph resolves itself; reporting it as broken would cry wolf on every fresh daemon")
		assert.Contains(t, detail, "still being built")
	})

	t.Run("answers the handshake but cannot serve", func(t *testing.T) {
		// THE case liveness cannot see: initialize is perfect, tool calls fail.
		fakeMCPDaemon(t, func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		state, detail := probeDaemonReadiness(context.Background())
		require.Equal(t, readyNotServing, state,
			"a daemon that handshakes and cannot serve must NOT read as healthy — that is the whole defect")
		assert.Contains(t, detail, "500")
	})

	t.Run("a wedged tool call is not-serving, not building", func(t *testing.T) {
		readinessProbeTimeout = 300 * time.Millisecond
		t.Cleanup(func() { readinessProbeTimeout = 10 * time.Second })
		// Block until released rather than sleeping a guess: the handler must
		// outlast the probe window without making the test wait for it. The
		// release MUST be registered after fakeMCPDaemon — t.Cleanup is LIFO,
		// so registering it first lets httptest's Close (which waits for
		// in-flight handlers) run before the unblock and deadlock the test.
		wedged := make(chan struct{})
		fakeMCPDaemon(t, func(w http.ResponseWriter) { <-wedged })
		t.Cleanup(func() { close(wedged) })
		state, _ := probeDaemonReadiness(context.Background())
		assert.Equal(t, readyNotServing, state,
			"a tool call that never returns is the wedged-build symptom the bead describes")
	})

	t.Run("no daemon at all is unknown, not broken", func(t *testing.T) {
		prev := projcfg.MacheHTTPURL
		projcfg.MacheHTTPURL = "http://127.0.0.1:1/mcp" // nothing listens
		t.Cleanup(func() { projcfg.MacheHTTPURL = prev })
		state, _ := probeDaemonReadiness(context.Background())
		assert.Equal(t, readyUnknown, state,
			"absence of a daemon is the daemon check's business; readiness must not double-report it")
	})
}

// TestCheckReadiness_StatusPerState pins how each state renders, because the
// value of the check is that an operator can tell them apart at a glance.
func TestCheckReadiness_StatusPerState(t *testing.T) {
	assert.Equal(t, statusOK, checkReadiness(readyServing, "").Status)
	assert.Equal(t, statusWarn, checkReadiness(readyBuilding, "building").Status,
		"transient state must warn, not fail")
	assert.Equal(t, statusWarn, checkReadiness(readyUnknown, "").Status)

	broken := checkReadiness(readyNotServing, "HTTP 500")
	assert.Equal(t, statusFail, broken.Status,
		"answering-but-not-serving is a failure: something LOOKS healthy and is not")
	assert.Contains(t, broken.Fix, "mache daemon restart")
}
