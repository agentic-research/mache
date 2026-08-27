//go:build darwin

package cmd

import (
	"bytes"
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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This is layer 3 of mache-96465d: the hermetic REAL-launchd E2E. Layers 1–2
// (unit seams, the stub-driven conformance table) run everywhere and cannot,
// by construction, see what launchd itself does: identity pinning at
// bootstrap, RunAtLoad semantics, real SIGTERM delivery, the codesigning
// SIGKILL that kept `mache daemon restart` broken for a week under a fully
// green suite. This test pays for that visibility by driving a REAL launchd
// job — a private label, a private port, an isolated HOME — through the
// upgrade story: install, serve, replace the binary with different bytes,
// restart, survive, stop, start again, and fail LOUDLY when the daemon can
// only crash-loop.
//
// Gated because it mutates the calling user's gui launchd domain (with a
// private label) and takes tens of seconds. Run it deliberately:
//
//	task test:launchd-e2e
//
// or MACHE_TEST_LAUNCHD_E2E=1 go test -run TestLaunchdLifecycleE2E ./cmd/.
func TestLaunchdLifecycleE2E(t *testing.T) {
	if os.Getenv("MACHE_TEST_LAUNCHD_E2E") != "1" {
		t.Skip("LOUD SKIP: real-launchd lifecycle E2E not run — set MACHE_TEST_LAUNCHD_E2E=1 " +
			"(or `task test:launchd-e2e`). Layers 1–2 cannot see identity pinning, " +
			"RunAtLoad, or SIGTERM semantics; only this test exercises them.")
	}
	if _, err := exec.LookPath("launchctl"); err != nil {
		t.Skip("launchctl not found; not a launchd host")
	}

	moduleRoot, err := filepath.Abs("..")
	require.NoError(t, err)

	// --- hermetic world -------------------------------------------------
	home := t.TempDir()
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "mache")
	label := fmt.Sprintf("com.agentic-research.mache.e2e.%d", os.Getpid())
	port := freePort(t)
	listen := fmt.Sprintf("localhost:%d", port)
	mcpURL := fmt.Sprintf("http://%s/mcp", listen)

	// The daemon must never touch the real ~/.mache: the plist bakes HOME, so
	// this is also the test OF that baking. Copy the real pinned leyline in so
	// nothing downloads.
	if src, err := os.ReadFile(realLeylinePath(t)); err == nil {
		lp := filepath.Join(home, ".mache", "bin", "leyline")
		require.NoError(t, os.MkdirAll(filepath.Dir(lp), 0o755))
		require.NoError(t, os.WriteFile(lp, src, 0o755))
	}

	env := func(extra ...string) []string {
		base := append(os.Environ(),
			"HOME="+home,
			"MACHE_DAEMON_LABEL="+label,
			"MACHE_DAEMON_LISTEN="+listen,
		)
		return append(base, extra...)
	}
	run := func(extraEnv []string, args ...string) (string, error) {
		cmd := exec.Command(binPath, args...)
		cmd.Env = append(env(), extraEnv...)
		var buf bytes.Buffer
		cmd.Stdout, cmd.Stderr = &buf, &buf
		err := cmd.Run()
		t.Logf("$ mache %s (err=%v)\n%s", strings.Join(args, " "), err, buf.String())
		return buf.String(), err
	}
	t.Cleanup(func() {
		// Best-effort teardown: the job must not outlive the test.
		_ = exec.Command("launchctl", "bootout",
			fmt.Sprintf("gui/%d/%s", os.Getuid(), label)).Run()
	})

	// --- two REAL builds with different bytes ---------------------------
	// Different -X values → different content → different ad-hoc CDHash.
	// That difference is exactly what turned `kickstart -k` into a kernel
	// SIGKILL loop (Code Signature Invalid) before the reload sequence.
	buildMache(t, moduleRoot, binPath, "e2e-generation-A")
	stagedB := filepath.Join(binDir, "mache.next")
	buildMache(t, moduleRoot, stagedB, "e2e-generation-B")

	// --- install, then start ----------------------------------------------
	// The documented flow: `mache init --global` installs the agent
	// (bootout+bootstrap, no kickstart), `mache daemon start` brings it up.
	// RunAtLoad does NOT fire on bootstrap (verified live, mache-706d8f), so
	// after init the correct state is loaded-idle, not running.
	out, err := run(nil, "init", "--global")
	require.NoError(t, err, "init --global must succeed:\n%s", out)
	assert.FileExists(t, filepath.Join(home, "Library", "LaunchAgents", label+".plist"),
		"the agent must be installed under the ISOLATED home, not the real one")

	out, err = run(nil, "daemon", "start")
	require.NoError(t, err, "start from loaded-idle must succeed:\n%s", out)
	require.True(t, mcpUp(mcpURL), "daemon must actually answer initialize after start")

	pidA := launchdPID(t, label)
	require.NotZero(t, pidA, "launchd must report a running pid")

	// --- a session, before the upgrade ----------------------------------
	sid := mcpInitialize(t, mcpURL)

	// --- upgrade: new bytes at the same path, then restart ---------------
	require.NoError(t, os.Rename(stagedB, binPath),
		"replace the binary in place — the upgrade launchd pinned identities against")
	out, err = run(nil, "daemon", "restart")
	require.NoError(t, err,
		"restart across a binary swap must succeed (the codesigning-kill regression):\n%s", out)
	require.True(t, mcpUp(mcpURL), "daemon must answer initialize after the upgrade restart")

	pidB := launchdPID(t, label)
	require.NotZero(t, pidB)
	assert.NotEqual(t, pidA, pidB, "a restart that keeps the old pid restarted nothing")

	// --- the session survives the upgrade --------------------------------
	// Same Mcp-Session-Id, no re-initialize. The new process must accept it
	// (stateless session IDs) — the mache-956488 continuity finding, pinned
	// here against real process replacement. tools/list needs no graph, so
	// this isolates TRANSPORT continuity from graph-rebuild time.
	assert.True(t, mcpToolsListOK(mcpURL, sid),
		"a pre-upgrade session must keep working against the post-upgrade daemon")

	// --- stop means stopped ----------------------------------------------
	out, err = run(nil, "daemon", "stop")
	require.NoError(t, err, "stop of a running daemon must succeed:\n%s", out)
	require.False(t, mcpUp(mcpURL), "the endpoint must be DOWN after stop claims success")
	// KeepAlive{SuccessfulExit:false} + clean SIGTERM exit = launchd must NOT
	// respawn it. Give a respawn a moment to disprove us.
	time.Sleep(2 * time.Second)
	assert.False(t, mcpUp(mcpURL), "launchd must not quietly respawn a cleanly stopped daemon")

	// --- start from loaded-idle ------------------------------------------
	out, err = run(nil, "daemon", "start")
	require.NoError(t, err, "start from loaded-idle must succeed:\n%s", out)
	require.True(t, mcpUp(mcpURL), "daemon must answer initialize after re-start")

	// --- a daemon that can only crash must fail LOUDLY --------------------
	// A listener held by the test occupies the port, so serve exits nonzero
	// on bind and launchd crash-loops it. `daemon start` must report failure
	// (no success claim) instead of hanging or lying.
	t.Run("crash loop is loud", func(t *testing.T) {
		crashLabel := label + ".crash"
		crashPort := freePort(t)
		hold, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", crashPort))
		require.NoError(t, err)
		defer func() { _ = hold.Close() }()
		t.Cleanup(func() {
			_ = exec.Command("launchctl", "bootout",
				fmt.Sprintf("gui/%d/%s", os.Getuid(), crashLabel)).Run()
		})

		crashEnv := []string{
			"MACHE_DAEMON_LABEL=" + crashLabel,
			"MACHE_DAEMON_LISTEN=" + fmt.Sprintf("localhost:%d", crashPort),
			"MACHE_DAEMON_SETTLE=8s",
		}
		out, err := run(crashEnv, "init", "--global")
		require.NoError(t, err, "installing the doomed agent must itself succeed:\n%s", out)

		out, err = run(crashEnv, "daemon", "start")
		require.Error(t, err, "a crash-looping daemon must fail the start verb:\n%s", out)
		assertNoUnverifiedClaims(t, out, err)
	})
}

// buildMache produces a real mache binary with generation-stamped bytes.
func buildMache(t *testing.T, moduleRoot, dst, generation string) {
	t.Helper()
	cmd := exec.Command("go", "build",
		"-ldflags", "-X github.com/agentic-research/mache/cmd.Commit="+generation,
		"-o", dst, ".")
	cmd.Dir = moduleRoot
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "go build (%s): %s", generation, out)
}

func realLeylinePath(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	return filepath.Join(home, ".mache", "bin", "leyline")
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// launchdPID reads the job's pid from `launchctl print`; 0 when not running.
func launchdPID(t *testing.T, label string) int {
	t.Helper()
	out, err := exec.Command("launchctl", "print",
		fmt.Sprintf("gui/%d/%s", os.Getuid(), label)).Output()
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(out), "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), " = ")
		if ok && key == "pid" {
			var pid int
			_, _ = fmt.Sscanf(val, "%d", &pid)
			return pid
		}
	}
	return 0
}

// mcpUp probes initialize the same way daemonEndpointUp does, from outside.
func mcpUp(url string) bool {
	body, _, err := mcpPost(url, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"e2e","version":"0"}}}`)
	return err == nil && strings.Contains(body, `"serverInfo"`)
}

// mcpInitialize opens a session and returns its Mcp-Session-Id.
func mcpInitialize(t *testing.T, url string) string {
	t.Helper()
	_, sid, err := mcpPost(url, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"e2e","version":"0"}}}`)
	require.NoError(t, err)
	require.NotEmpty(t, sid, "streamable HTTP must issue a session id")
	return sid
}

// mcpToolsListOK reports whether tools/list succeeds on an existing session.
func mcpToolsListOK(url, sid string) bool {
	body, _, err := mcpPost(url, sid, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	return err == nil && strings.Contains(body, `"tools"`)
}

// mcpPost is a minimal streamable-HTTP request: returns body and session id.
func mcpPost(url, sid, payload string) (string, string, error) {
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(payload))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}
	if resp.StatusCode >= 400 {
		return string(raw), "", fmt.Errorf("http %d: %s", resp.StatusCode, raw)
	}
	return string(raw), resp.Header.Get("Mcp-Session-Id"), nil
}
