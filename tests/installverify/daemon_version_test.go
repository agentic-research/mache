package installverify

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// supervisedDaemonURL is where `mache init --global` keeps a shared daemon.
// Hard-coded rather than derived because this gate deliberately checks the
// well-known address a real editor connects to, not one this test chose.
const supervisedDaemonURL = "http://localhost:7532/mcp"

// daemonVerifyEnv opts in to the live-daemon check. See the skip in
// TestSupervisedDaemonServesTheInstalledBinary for why this is not default-on.
const daemonVerifyEnv = "MACHE_VERIFY_DAEMON"

// TestSupervisedDaemonServesTheInstalledBinary closes the gap that made a
// v0.20.0 release land without taking effect.
//
// THE DEFECT. Replacing the binary on disk does not re-exec a supervisor that
// is already running — it keeps serving the inode it exec'd. Measured
// 2026-07-30 right after the v0.20.0 release:
//
//	installed  ~/.local/bin/mache   0.20.0-5-g9c0e374
//	daemon on  localhost:7532       0.19.0-10-g1c3d812   <- 10 commits behind
//
// Every MCP session was served pre-v0.20.0 code, including the P0
// construct-loss bug (mache-c725e9) that release exists to fix, and nothing
// anywhere reported the skew. `mache daemon restart` now runs from both
// install paths, but that only prevents the skew when install is the thing
// that changed the binary — a `go build -o`, a brew upgrade, or a manual cp
// reintroduces it. This asserts the invariant itself rather than one of the
// paths that can break it (mache-e418ce).
//
// SKIPS WHEN NO DAEMON IS RUNNING. That is the state of any CI runner and of
// a developer who never ran `mache init --global`; there is no skew to detect
// and nothing to report. The gate is meaningful exactly where the defect is
// possible — a machine actually running a supervised daemon — and must not
// manufacture one, since starting a daemon is a decision this test does not
// get to make.
func TestSupervisedDaemonServesTheInstalledBinary(t *testing.T) {
	// OPT-IN ONLY. This is the one test in this package that inspects MACHINE
	// STATE rather than the binary under test, so without a gate it runs on a
	// plain `go test ./...` — which `task test` performs — and reports a
	// stale-daemon problem as a source-tree test failure. Someone whose daemon
	// went stale then cannot get `task check` green, and the fix is not in the
	// diff they are working on.
	//
	// It also contributes nothing in CI: no runner has a supervised daemon, and
	// restartDaemonAgent correctly refuses to conjure one, so it would skip at
	// the listen probe anyway. Gating it costs no coverage where coverage was
	// possible, and stops it breaking a developer's unrelated run. Reached via
	// `task daemon:verify` (found by an external review of PR #589).
	if os.Getenv(daemonVerifyEnv) == "" {
		t.Skipf("%s unset — run `task daemon:verify` (this inspects the live daemon, not the build)", daemonVerifyEnv)
	}
	if !daemonListening(supervisedDaemonURL) {
		t.Skip("no supervised daemon on localhost:7532 — nothing to compare (run `mache init --global` to have one)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c := newMCPClient(supervisedDaemonURL)
	resp, hdr, err := c.post(ctx, "initialize", map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "mache-daemon-version-gate", "version": "1"},
	})
	require.NoError(t, err, "the supervised daemon answered the port but not MCP initialize")
	require.NotNil(t, resp, "initialize returned no result")
	_ = hdr

	var init struct {
		ServerInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	require.NoError(t, json.Unmarshal(resp.Result, &init), "decode initialize result")
	require.Equal(t, "mache", init.ServerInfo.Name,
		"something other than mache is bound to 7532")
	require.NotEmpty(t, init.ServerInfo.Version, "daemon reported no version")

	// COMPARE AGAINST THE SUPERVISOR'S OWN BINARY, not this gate's
	// binary-under-test. The daemon's contract is "I am running the binary my
	// plist/unit points at" — nothing promises it matches an arbitrary build
	// someone is testing. Comparing against MACHE_VERIFY_BINARY made this fail
	// on any working tree with uncommitted changes, which is a false positive
	// generator: a required gate that cries wolf gets disabled, and then the
	// real skew it exists to catch sails through.
	supervised := supervisorBinary(t)
	if supervised == "" {
		t.Skip("no supervisor definition found — cannot establish which binary the daemon should be running")
	}
	out := runner{}.mustRun(t, supervised, "version")
	installed, ok := parseVersionLine(out.stdout)
	require.True(t, ok, "could not parse `%s version` output:\n%s", supervised, out.stdout)

	assert.Equal(t, installed, init.ServerInfo.Version,
		"the supervised daemon is serving a DIFFERENT build than the binary its own "+
			"supervisor points at (%s).\n  that binary: %s\n  daemon:      %s\n"+
			"Replacing the binary does not restart a running supervisor. Run `mache daemon restart`.",
		supervised, installed, init.ServerInfo.Version)
}

// supervisorBinary returns the mache path the platform supervisor is
// configured to launch, or "" when no supervisor definition exists.
//
// This is the ONLY binary the running daemon can be expected to match. Reading
// it from the definition (rather than assuming ~/.local/bin/mache) means the
// gate stays correct for someone who installed with --bin-dir elsewhere.
func supervisorBinary(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	switch runtime.GOOS {
	case "darwin":
		// First <string> inside ProgramArguments is argv[0].
		b, err := os.ReadFile(filepath.Join(home, "Library", "LaunchAgents",
			"com.agentic-research.mache.plist"))
		if err != nil {
			return ""
		}
		_, rest, found := strings.Cut(string(b), "<key>ProgramArguments</key>")
		if !found {
			return ""
		}
		_, rest, found = strings.Cut(rest, "<string>")
		if !found {
			return ""
		}
		path, _, found := strings.Cut(rest, "</string>")
		if !found {
			return ""
		}
		return strings.TrimSpace(path)
	case "linux":
		b, err := os.ReadFile(filepath.Join(home, ".config", "systemd", "user", "mache.service"))
		if err != nil {
			return ""
		}
		for _, line := range strings.Split(string(b), "\n") {
			rest, ok := strings.CutPrefix(strings.TrimSpace(line), "ExecStart=")
			if !ok {
				continue
			}
			field, _, _ := strings.Cut(strings.TrimSpace(rest), " ")
			return strings.Trim(field, `"`)
		}
	}
	return ""
}

// daemonListening reports whether something accepts TCP on the daemon's
// address. A cheap dial rather than an HTTP round trip: this only decides
// whether the assertion is applicable, and a port that refuses connections
// unambiguously means no supervised daemon.
func daemonListening(url string) bool {
	addr := strings.TrimPrefix(url, "http://")
	if i := strings.Index(addr, "/"); i >= 0 {
		addr = addr[:i]
	}
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
