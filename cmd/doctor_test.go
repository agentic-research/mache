package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// isolateHome points os.UserHomeDir at a temp tree so the arena and registry
// checks observe a world this test constructed. Both go through
// os.UserHomeDir, which honours $HOME on unix.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".mache"), 0o700))
	return home
}

// TestServerVersionFromMCPReply_BothFramings is the parser's real job. The
// transport may answer with a bare JSON body or with SSE, and a doctor whose
// verdict flips on that distinction would report "daemon down" for a healthy
// daemon — the exact false alarm this command exists to eliminate.
func TestServerVersionFromMCPReply_BothFramings(t *testing.T) {
	plain := `{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"mache","version":"1.2.3"}}}`
	sse := "event: message\ndata: " + plain + "\n\n"

	for name, raw := range map[string]string{"plain JSON": plain, "SSE-framed": sse} {
		t.Run(name, func(t *testing.T) {
			got, err := serverVersionFromMCPReply([]byte(raw))
			require.NoError(t, err)
			assert.Equal(t, "1.2.3", got)
		})
	}
}

// TestServerVersionFromMCPReply_RefusesUnusableReplies pins that a malformed or
// version-less reply is an ERROR, not an empty string. Returning "" would make
// the skew check compare against nothing and silently report agreement.
func TestServerVersionFromMCPReply_RefusesUnusableReplies(t *testing.T) {
	for name, raw := range map[string]string{
		"not json":      "<html>502 Bad Gateway</html>",
		"no serverInfo": `{"jsonrpc":"2.0","id":1,"result":{}}`,
		"empty version": `{"result":{"serverInfo":{"version":""}}}`,
		"jsonrpc error": `{"jsonrpc":"2.0","id":1,"error":{"code":-32600}}`,
		"empty body":    "",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := serverVersionFromMCPReply([]byte(raw))
			assert.Error(t, err, "an unusable reply must not read as a healthy daemon")
		})
	}
}

// TestCheckVersionSkew_FiresOnlyWhenComparable is the check that would have
// caught the reported incident (a stale daemon serving after an upgrade), and
// the one most at risk of crying wolf. A bare `go build` cannot know its git
// distance, so cmd.Version reports the release BASE — comparing that against a
// daemon's ldflags-stamped string would fail on every developer run.
func TestCheckVersionSkew_FiresOnlyWhenComparable(t *testing.T) {
	t.Run("bare build cannot compare", func(t *testing.T) {
		defer swapBuildVersion(t, "")()
		got := checkVersionSkew("9.9.9-stamped", nil)
		assert.Equal(t, statusWarn, got.Status,
			"a bare build must not claim skew it cannot actually observe")
		assert.Contains(t, got.Detail, "bare build")
	})

	t.Run("stamped build detects real skew", func(t *testing.T) {
		defer swapBuildVersion(t, "1.0.0-5-gabc")()
		got := checkVersionSkew("1.0.0-9-gdef", nil)
		assert.Equal(t, statusFail, got.Status)
		assert.Contains(t, got.Detail, "OLD code")
		assert.NotEmpty(t, got.Fix, "a failing check must name its remediation")
	})

	t.Run("stamped build agrees", func(t *testing.T) {
		defer swapBuildVersion(t, "1.0.0-5-gabc")()
		assert.Equal(t, statusOK, checkVersionSkew(Version, nil).Status)
	})

	t.Run("no daemon is warn, not fail", func(t *testing.T) {
		got := checkVersionSkew("", errors.New("connection refused"))
		assert.Equal(t, statusWarn, got.Status,
			"an absent daemon is a daemon problem, not a skew problem — it must not double-report")
	})
}

// swapBuildVersion sets the ldflags-injected build marker (and the derived
// Version) for one test, restoring both afterwards.
func swapBuildVersion(t *testing.T, v string) func() {
	t.Helper()
	oldBuild, oldVersion := buildVersion, Version
	buildVersion = v
	Version = resolveBuildVersion()
	return func() { buildVersion, Version = oldBuild, oldVersion }
}

// TestCheckDaemon_DownIsWarnWithRemediation covers the "no daemon" path. It is
// deliberately a warn: not every invocation wants a daemon, so it must not set
// the exit code — but it must still say how to start one.
func TestCheckDaemon_DownIsWarnWithRemediation(t *testing.T) {
	got := checkDaemon("", errors.New("connection refused"))
	assert.Equal(t, statusWarn, got.Status)
	assert.Contains(t, got.Fix, "mache init --global")
	assert.NotContains(t, strings.ToLower(got.Detail), "deadline exceeded",
		"the incident this command exists for presented as a bare timeout; doctor must never restate one as its own diagnosis")
}

// TestCheckArena_DistinguishesEveryState walks the four arena states. The
// bound-elsewhere case is the one that previously surfaced only as daemon
// features going quiet: ley-line refuses to warm-start when the arena's
// recorded source_root disagrees, and every caller degrades rather than
// propagating the reason.
func TestCheckArena_DistinguishesEveryState(t *testing.T) {
	writeArena := func(t *testing.T, home, sourceRoot string) {
		t.Helper()
		arena := filepath.Join(home, ".mache", "default.arena")
		require.NoError(t, os.WriteFile(arena, []byte("x"), 0o644))
		body, err := json.Marshal(map[string]string{"source_root": sourceRoot, "cdc_target": "source-blobs"})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(arena+".mache-config.json", body, 0o644))
	}

	t.Run("no arena is fine", func(t *testing.T) {
		isolateHome(t)
		got := checkArena(t.TempDir())
		assert.Equal(t, statusOK, got.Status)
		assert.Contains(t, got.Detail, "cold start")
	})

	t.Run("arena without a record warns", func(t *testing.T) {
		home := isolateHome(t)
		require.NoError(t, os.WriteFile(filepath.Join(home, ".mache", "default.arena"), []byte("x"), 0o644))
		got := checkArena(t.TempDir())
		assert.Equal(t, statusWarn, got.Status)
	})

	t.Run("bound to another tree fails with the reason", func(t *testing.T) {
		home := isolateHome(t)
		other := t.TempDir()
		writeArena(t, home, other)
		here := t.TempDir()

		got := checkArena(here)
		require.Equal(t, statusFail, got.Status,
			"a warm-start refusal must be reported, not left to surface as silence")
		assert.Contains(t, got.Detail, "refuse to warm-start")
		assert.Contains(t, got.Detail, other, "the detail must name the tree actually holding the arena")
		assert.NotEmpty(t, got.Fix)
	})

	t.Run("bound to this tree passes", func(t *testing.T) {
		home := isolateHome(t)
		here := t.TempDir()
		writeArena(t, home, here)
		assert.Equal(t, statusOK, checkArena(here).Status)
	})
}

// TestCheckProjectRegistration_UnregisteredIsNamedNotTimedOut is the honesty
// fix for "workspace root unavailable (context deadline exceeded)" — a message
// describing the symptom rather than the cause.
//
// It must also not over-correct into the opposite lie. An unregistered project
// is NOT broken: the daemon asks the client for its root over roots/list and
// only needs the ?project= token when the client cannot answer. Claude Code
// answers — verified against a live daemon, where a session resolved a repo
// that had no registry entry and no .claude/mcp.json. Reporting FAIL here made
// `mache doctor` exit 1 on a healthy tree and sent people to run `mache init`
// per directory, and per BRANCH once git worktrees are involved.
func TestCheckProjectRegistration_UnregisteredIsNamedNotTimedOut(t *testing.T) {
	home := isolateHome(t)
	cwd := t.TempDir()

	got := checkProjectRegistration(cwd)
	require.Equal(t, statusWarn, got.Status,
		"unregistered is a note, not a failure: roots/list clients resolve without it")
	assert.Contains(t, got.Detail, "not pre-registered")
	assert.Contains(t, got.Detail, "roots/list",
		"the reader has to know WHICH clients need the remedy")
	assert.Contains(t, got.Fix, "mache init")
	assert.NotContains(t, got.Detail, "will fail",
		"doctor cannot know which client will connect, so it must not predict failure")
	assert.NotContains(t, got.Detail, "deadline",
		"an unregistered project must be reported as unregistered, never as a timeout")

	// And the positive case, through the same registry file the daemon reads.
	reg := filepath.Join(home, ".mache", "projects.json")
	body, err := json.Marshal(map[string]string{"sometoken": cwd})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(reg, body, 0o600))

	// Unconditional on purpose: guarding this behind a stat would let the
	// positive case silently stop running if projectRegistryFile ever changed,
	// and a test that cannot fire is worse than no test.
	require.FileExists(t, reg, "fixture must write the registry the loader actually reads")
	assert.Equal(t, statusOK, checkProjectRegistration(cwd).Status, "a registered cwd must pass")
}

// TestDoctorExit_OnlyFailuresSetTheExitCode pins the three-valued contract:
// warn is informational, so `mache doctor` in a script means "is anything
// actually broken", not "is anything unusual".
func TestDoctorExit_OnlyFailuresSetTheExitCode(t *testing.T) {
	assert.NoError(t, doctorExit([]check{{Status: statusOK}, {Status: statusWarn}}))
	assert.Error(t, doctorExit([]check{{Status: statusOK}, {Status: statusFail}}))
}

// TestEveryFailingCheckNamesARemediation is the cross-cutting invariant: a
// check that can fail without saying what to do just relocates the guessing
// that this command exists to remove.
func TestEveryFailingCheckNamesARemediation(t *testing.T) {
	defer swapBuildVersion(t, "1.0.0-5-gabc")()
	home := isolateHome(t)
	other := t.TempDir()
	arena := filepath.Join(home, ".mache", "default.arena")
	require.NoError(t, os.WriteFile(arena, []byte("x"), 0o644))
	body, _ := json.Marshal(map[string]string{"source_root": other})
	require.NoError(t, os.WriteFile(arena+".mache-config.json", body, 0o644))

	for _, c := range []check{
		checkVersionSkew("1.0.0-9-gdef", nil),
		checkArena(t.TempDir()),
	} {
		require.Equal(t, statusFail, c.Status, "fixture must actually produce a failure for %q", c.Name)
		assert.NotEmpty(t, c.Fix, "failing check %q must name a remediation", c.Name)
	}
}

// TestCheckPinnedLeyline_MissingBinaryFailsWithoutDownloading is the fifth
// failure mode. Two things must hold at once, and they pull in opposite
// directions: an unresolvable pin has to FAIL loudly (a projection built by the
// wrong leyline diverges from CI silently — the exact class mache-608a3c), and
// the check must not fetch anything while establishing that. A diagnostic that
// repairs the world cannot describe it.
func TestCheckPinnedLeyline_MissingBinaryFailsWithoutDownloading(t *testing.T) {
	// Isolate both places ResolveBinary looks: $PATH and ~/.mache/bin.
	empty := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", empty)

	got := checkPinnedLeyline()

	require.Equal(t, statusFail, got.Status,
		"an unresolvable pin must fail — a wrong-version leyline produces a projection that diverges from CI without saying so")
	assert.Contains(t, got.Detail, "not resolvable")
	assert.Contains(t, got.Fix, "mache install", "a failing check must name its remediation")

	// The load-bearing half: nothing was downloaded into the isolated home.
	home := os.Getenv("HOME")
	if entries, err := os.ReadDir(filepath.Join(home, ".mache", "bin")); err == nil {
		assert.Empty(t, entries,
			"doctor must not fetch a binary while diagnosing; it reports the world, it does not change it")
	}
}

// TestWorkspaceRootFor_ResolvesTheRegisteredUnit is the fix for a false alarm
// that shipped in the first cut: run from cmd/, doctor reported the project
// "NOT registered" and told the operator to run `mache init` there — which
// would mint a SECOND token for a nested path and make the registry genuinely
// wrong. MCP clients advertise a workspace ROOT and mache registers that; the
// process working directory is not the question anyone asked.
func TestWorkspaceRootFor_ResolvesTheRegisteredUnit(t *testing.T) {
	t.Run("finds the root from a nested directory", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0o755))
		nested := filepath.Join(root, "cmd", "deep")
		require.NoError(t, os.MkdirAll(nested, 0o755))

		assert.Equal(t, root, workspaceRootFor(nested))
	})

	t.Run("worktrees and submodules count — .git is a FILE there", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: /elsewhere\n"), 0o644))
		nested := filepath.Join(root, "pkg")
		require.NoError(t, os.MkdirAll(nested, 0o755))

		assert.Equal(t, root, workspaceRootFor(nested),
			"a worktree checkout is a real workspace; missing it would resurrect the false alarm")
	})

	t.Run("falls back to the input rather than escaping to /", func(t *testing.T) {
		orphan := t.TempDir() // no .git anywhere it will find inside the temp tree
		got := workspaceRootFor(orphan)
		assert.True(t, got == orphan || strings.HasPrefix(orphan, got),
			"a directory outside any repo must report itself, never walk up to the filesystem root and claim that")
	})
}

// TestEmitDoctorJSON_StaysParseableWhenChecksFail pins half of a two-part fix.
// The writer must emit ONLY JSON; the other half is cmd.Execute writing its
// error to stderr rather than stdout. Together they keep `mache doctor --json`
// machine-readable in the failure case — which is the case an agent most needs
// to parse, and the one where it was previously invalid.
func TestEmitDoctorJSON_StaysParseableWhenChecksFail(t *testing.T) {
	var buf bytes.Buffer
	err := emitDoctorJSON(&buf, []check{
		{Name: "ok-one", Status: statusOK, Detail: "fine"},
		{Name: "bad-one", Status: statusFail, Detail: "broken", Fix: "do the thing"},
	})
	require.Error(t, err, "a failing check must still set the exit code")

	var decoded struct {
		Checks []check `json:"checks"`
		Failed int     `json:"failed"`
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded),
		"the JSON writer must emit nothing but JSON, even when reporting a failure")
	assert.Equal(t, 1, decoded.Failed)
	assert.Len(t, decoded.Checks, 2)
}

// TestCheckClientToken_SeparatesRegistrationFromReachability covers the gap
// that validation exposed: doctor reported six green checks while
// find_definition returned "workspace root unavailable (context deadline
// exceeded)". The project WAS registered — the client's URL just carried no
// token and its roots/list never answered. Registered and reachable are
// different questions.
func TestCheckClientToken_SeparatesRegistrationFromReachability(t *testing.T) {
	write := func(t *testing.T, path, url string) {
		t.Helper()
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		body := fmt.Sprintf(`{"mcpServers":{"mache":{"type":"http","url":%q}}}`, url)
		require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	}

	t.Run("bare URL warns and never gates", func(t *testing.T) {
		root := t.TempDir()
		write(t, filepath.Join(root, ".mcp.json"), "http://localhost:7532/mcp")

		got := checkClientToken(root)
		assert.Equal(t, statusWarn, got.Status,
			"a client that answers roots/list works without a token, so this must not fail the exit code")
		assert.Contains(t, got.Detail, "no ?project= token")
		assert.Contains(t, got.Fix, "mache init")
	})

	t.Run("token present passes", func(t *testing.T) {
		root := t.TempDir()
		write(t, filepath.Join(root, ".claude", "mcp.json"), "http://localhost:7532/mcp?project=deadbeef")
		assert.Equal(t, statusOK, checkClientToken(root).Status)
	})

	t.Run("a tokenless file alongside a tokened one still passes", func(t *testing.T) {
		root := t.TempDir()
		write(t, filepath.Join(root, ".mcp.json"), "http://localhost:7532/mcp")
		write(t, filepath.Join(root, ".claude", "mcp.json"), "http://localhost:7532/mcp?project=deadbeef")
		assert.Equal(t, statusOK, checkClientToken(root).Status,
			"the committed .mcp.json CANNOT carry a machine-specific token; the per-machine file is what resolves")
	})

	t.Run("no config at all warns rather than claiming health", func(t *testing.T) {
		assert.Equal(t, statusWarn, checkClientToken(t.TempDir()).Status)
	})

	t.Run("unreadable config is no opinion, not a verdict", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(root, ".mcp.json"), []byte("{not json"), 0o644))
		got := checkClientToken(root)
		assert.Equal(t, statusWarn, got.Status)
		assert.NotContains(t, got.Detail, "no ?project= token",
			"a file doctor cannot parse must not be reported as a file lacking a token")
	})
}
