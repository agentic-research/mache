//go:build darwin

package cmd

import (
	"bytes"
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

	"github.com/agentic-research/mache/internal/testport"
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

	w := newE2EWorld(t)

	e2eInstallAndStart(t, w)
	sid := mcpInitialize(t, w.mcpURL)
	pidA := launchdPID(t, w.label)
	require.NotZero(t, pidA, "launchd must report a running pid")

	e2eUpgradeRestart(t, w)
	pidB := launchdPID(t, w.label)
	require.NotZero(t, pidB)
	assert.NotEqual(t, pidA, pidB, "a restart that keeps the old pid restarted nothing")

	// Same Mcp-Session-Id, no re-initialize. The new process must accept it
	// (stateless session IDs) — the mache-956488 continuity finding, pinned
	// here against real process replacement. tools/list needs no graph, so
	// this isolates TRANSPORT continuity from graph-rebuild time.
	assert.True(t, mcpToolsListOK(w.mcpURL, sid),
		"a pre-upgrade session must keep working against the post-upgrade daemon")

	e2eStopThenStart(t, w)

	// --- a daemon that can only crash must fail LOUDLY --------------------
	// A listener held by the test occupies the port, so serve exits nonzero
	// on bind and launchd crash-loops it. `daemon start` must report failure
	// (no success claim) instead of hanging or lying.
	t.Run("crash loop is loud", func(t *testing.T) {
		crashLabel := w.label + ".crash"
		crashPort := testport.Free(t)
		hold, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", crashPort))
		require.NoError(t, err)
		defer func() { _ = hold.Close() }()
		t.Cleanup(func() { bootoutAndWait(t, crashLabel) })

		// Tight breaker bounds so the loop can be watched to its END inside a
		// test: production trips after 5 crashes / 2min, which at launchd's
		// 10s ThrottleInterval is ~50s of waiting. The bounds are baked into
		// the generated plist, so setting them here reaches the daemon.
		crashEnv := []string{
			"MACHE_DAEMON_LABEL=" + crashLabel,
			"MACHE_DAEMON_LISTEN=" + fmt.Sprintf("localhost:%d", crashPort),
			"MACHE_DAEMON_SETTLE=8s",
			"MACHE_DAEMON_BREAKER_BURST=2",
			"MACHE_DAEMON_BREAKER_WINDOW=10m",
		}
		out, err := w.run(t, crashEnv, "init", "--global")
		require.NoError(t, err, "installing the doomed agent must itself succeed:\n%s", out)

		out, err = w.run(t, crashEnv, "daemon", "start")
		require.Error(t, err, "a crash-looping daemon must fail the start verb:\n%s", out)
		assertNoUnverifiedClaims(t, out, err)

		// The loop must END. Without the breaker (mache-956488) launchd
		// relaunches a doomed binary every ThrottleInterval forever; with it,
		// the daemon observes its own unclean starts and exits ZERO, which is
		// how both supervisors are told to stop.
		startsFile := filepath.Join(w.home, ".mache", "daemon-starts.json")
		recorded := awaitRecordedStartsQuiet(t, startsFile)
		require.GreaterOrEqual(t, recorded, 2,
			"the daemon must have actually attempted to start (and crashed) before the breaker could bound it")
		// With BURST=2 the breaker trips on the third start. An UNBOUNDED loop
		// would have recorded ~16 by now (measured), so this separates
		// bounded from still-running by a wide margin.
		assert.Lessf(t, recorded, 6,
			"the respawn loop must be BOUNDED by the breaker, not still running: %d starts recorded", recorded)

		// And it stays stopped: launchd must not be holding a live process.
		assert.Zero(t, launchdPID(t, crashLabel),
			"after the breaker trips, no supervised process may remain")
	})
}

// e2eWorld is the hermetic sandbox one E2E run lives in: an isolated HOME, a
// private launchd label, a private port, and a binary path the test upgrades
// in place.
type e2eWorld struct {
	home, label, listen, mcpURL string
	binPath, stagedB            string
}

// newE2EWorld builds the sandbox and the two REAL mache binaries. Different
// -X values → different content → different ad-hoc CDHash. That difference is
// exactly what turned `kickstart -k` into a kernel SIGKILL loop (Code
// Signature Invalid) before the reload sequence.
func newE2EWorld(t *testing.T) *e2eWorld {
	t.Helper()
	moduleRoot, err := filepath.Abs("..")
	require.NoError(t, err)

	home := t.TempDir()
	binDir := t.TempDir()
	w := &e2eWorld{
		home:    home,
		label:   fmt.Sprintf("com.agentic-research.mache.e2e.%d", os.Getpid()),
		binPath: filepath.Join(binDir, "mache"),
		stagedB: filepath.Join(binDir, "mache.next"),
	}
	w.listen = fmt.Sprintf("localhost:%d", testport.Free(t))
	w.mcpURL = fmt.Sprintf("http://%s/mcp", w.listen)

	// The daemon must never touch the real ~/.mache: the plist bakes HOME, so
	// this is also the test OF that baking. Copy the real pinned leyline in so
	// nothing downloads.
	if src, err := os.ReadFile(realLeylinePath(t)); err == nil {
		lp := filepath.Join(home, ".mache", "bin", "leyline")
		require.NoError(t, os.MkdirAll(filepath.Dir(lp), 0o755))
		require.NoError(t, os.WriteFile(lp, src, 0o755))
	}

	buildMache(t, moduleRoot, w.binPath, "e2e-generation-A")
	buildMache(t, moduleRoot, w.stagedB, "e2e-generation-B")

	t.Cleanup(func() {
		// The job must not outlive the test — and must be GONE before the
		// sandbox HOME is removed underneath it.
		bootoutAndWait(t, w.label)
	})
	return w
}

// run invokes the sandboxed binary with the sandbox environment.
func (w *e2eWorld) run(t *testing.T, extraEnv []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(w.binPath, args...)
	cmd.Env = append(append(os.Environ(),
		"HOME="+w.home,
		"MACHE_DAEMON_LABEL="+w.label,
		"MACHE_DAEMON_LISTEN="+w.listen,
	), extraEnv...)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	t.Logf("$ mache %s (err=%v)\n%s", strings.Join(args, " "), err, buf.String())
	return buf.String(), err
}

// e2eInstallAndStart walks the documented flow: `mache init --global`
// installs the agent (bootout+bootstrap, no kickstart), `mache daemon start`
// brings it up. RunAtLoad does NOT fire on bootstrap (verified live,
// mache-706d8f), so after init the correct state is loaded-idle, not running.
func e2eInstallAndStart(t *testing.T, w *e2eWorld) {
	t.Helper()
	out, err := w.run(t, nil, "init", "--global")
	require.NoError(t, err, "init --global must succeed:\n%s", out)
	assert.FileExists(t, filepath.Join(w.home, "Library", "LaunchAgents", w.label+".plist"),
		"the agent must be installed under the ISOLATED home, not the real one")

	out, err = w.run(t, nil, "daemon", "start")
	require.NoError(t, err, "start from loaded-idle must succeed:\n%s", out)
	require.True(t, mcpUp(w.mcpURL), "daemon must actually answer initialize after start")
}

// e2eUpgradeRestart replaces the binary in place — the upgrade launchd pinned
// identities against — and restarts across the swap (the codesigning-kill
// regression, mache-706d8f).
func e2eUpgradeRestart(t *testing.T, w *e2eWorld) {
	t.Helper()
	require.NoError(t, os.Rename(w.stagedB, w.binPath),
		"replace the binary in place — the upgrade launchd pinned identities against")
	out, err := w.run(t, nil, "daemon", "restart")
	require.NoError(t, err,
		"restart across a binary swap must succeed (the codesigning-kill regression):\n%s", out)
	require.True(t, mcpUp(w.mcpURL), "daemon must answer initialize after the upgrade restart")
}

// e2eStopThenStart pins that stop STICKS (KeepAlive{SuccessfulExit:false} +
// clean SIGTERM exit = launchd must not respawn) and that start revives a
// loaded-idle job.
func e2eStopThenStart(t *testing.T, w *e2eWorld) {
	t.Helper()
	out, err := w.run(t, nil, "daemon", "stop")
	require.NoError(t, err, "stop of a running daemon must succeed:\n%s", out)
	require.False(t, mcpUp(w.mcpURL), "the endpoint must be DOWN after stop claims success")
	assert.Never(t, func() bool { return mcpUp(w.mcpURL) }, 2*time.Second, 200*time.Millisecond,
		"launchd must not quietly respawn a cleanly stopped daemon")

	out, err = w.run(t, nil, "daemon", "start")
	require.NoError(t, err, "start from loaded-idle must succeed:\n%s", out)
	require.True(t, mcpUp(w.mcpURL), "daemon must answer initialize after re-start")
}

// bootoutAndWait removes a launchd job and waits for it to actually leave the
// domain before returning.
//
// Fire-and-forget bootout is not enough in teardown for the same reason it is
// not enough in the reload sequence: it returns once removal is INITIATED. A
// still-terminating daemon kept writing its breaker state into the sandbox
// HOME while t.TempDir's RemoveAll was deleting it, failing the whole test
// with "directory not empty" AFTER every assertion had passed.
func bootoutAndWait(t *testing.T, label string) {
	t.Helper()
	target := fmt.Sprintf("gui/%d/%s", os.Getuid(), label)
	_ = exec.Command("launchctl", "bootout", target).Run()
	assert.Eventuallyf(t, func() bool {
		return exec.Command("launchctl", "print", target).Run() != nil
	}, 30*time.Second, 200*time.Millisecond,
		"%s did not leave the launchd domain after bootout — a daemon that outlives its test "+
			"will keep writing into the sandbox as it is deleted", label)
}

// awaitRecordedStartsQuiet watches the breaker's start log until it stops
// growing, and returns the final count.
//
// "Quiet" must outlast the supervisor's own retry interval, or the GAP
// BETWEEN RESPAWNS reads as the loop having ended. The plist sets
// ThrottleInterval=10, so quiet is measured over 25s. Getting this wrong is
// not hypothetical: the first version polled for 6s of no-change and passed
// happily against a still-looping daemon — caught only by mutating the trip
// verdict to always-false.
func awaitRecordedStartsQuiet(t *testing.T, startsFile string) int {
	t.Helper()
	const quietFor = 25 * time.Second
	count := 0
	lastChange := time.Now()
	require.Eventually(t, func() bool {
		if n := countRecordedStarts(t, startsFile); n != count {
			count = n
			lastChange = time.Now()
		}
		return count > 0 && time.Since(lastChange) > quietFor
	}, 150*time.Second, 2*time.Second,
		"the daemon never recorded a start, or never stopped respawning")
	return count
}

// countRecordedStarts reads the crash-loop breaker's state file. A missing or
// half-written file counts as zero — the caller watches for the count to go
// QUIET, so a transient read error must not read as "loop ended".
func countRecordedStarts(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var st struct {
		Starts []struct {
			At int64 `json:"at"`
		} `json:"starts"`
	}
	if json.Unmarshal(data, &st) != nil {
		return 0
	}
	return len(st.Starts)
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
