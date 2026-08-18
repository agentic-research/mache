package cmd

import (
	"encoding/json"
	"errors"
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
func TestCheckProjectRegistration_UnregisteredIsNamedNotTimedOut(t *testing.T) {
	home := isolateHome(t)
	cwd := t.TempDir()

	got := checkProjectRegistration(cwd)
	require.Equal(t, statusFail, got.Status)
	assert.Contains(t, got.Detail, "NOT registered")
	assert.Contains(t, got.Fix, "mache init")
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
		checkProjectRegistration(t.TempDir()),
	} {
		require.Equal(t, statusFail, c.Status, "fixture must actually produce a failure for %q", c.Name)
		assert.NotEmpty(t, c.Fix, "failing check %q must name a remediation", c.Name)
	}
}
