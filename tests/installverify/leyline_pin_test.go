package installverify

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentic-research/mache/internal/leyline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// expectLeylineEnv pins the leyline version the installed mache must reach,
// for release-verification mode (a downloaded v0.19.0 asset pins a different
// leyline than this tree does). Unset, the expectation is this tree's pin.
const expectLeylineEnv = "MACHE_VERIFY_EXPECT_LEYLINE"

// shadowVersion is the version the deliberately-stale stub reports. It is the
// version measured in bead mache-19326d: ~/.local/bin/leyline was 0.10.3 while
// the pin was v0.13.0, so `leyline cdc enable` at a shell ran a parser three
// minors off from the one that built the .db. Both binaries are named
// `leyline`; neither warns.
const shadowVersion = "0.10.3"

// firstSemver returns the first MAJOR.MINOR.PATCH field of a version line such
// as "leyline 0.13.0 (open)", or "" when there is none.
//
// A field scan with a shape check, not a pattern: this mirrors
// internal/leyline's own extractSemver, which is unexported, and keeps the gate
// on the repo's structural-over-regex side (bead-documented preference, and the
// regexp ratchet in internal/lint enforces it).
func firstSemver(s string) string {
	for _, field := range strings.Fields(s) {
		candidate := strings.TrimPrefix(strings.Trim(field, "(),"), "v")
		if isSemverBase(candidate) {
			return candidate
		}
	}
	return ""
}

// expectedLeylinePin is the leyline version the installed mache must resolve.
func expectedLeylinePin(t *testing.T) string {
	t.Helper()
	if v := strings.TrimSpace(os.Getenv(expectLeylineEnv)); v != "" {
		return strings.TrimPrefix(v, "v")
	}
	return strings.TrimPrefix(leyline.BinaryVersion, "v")
}

// leylineSemver runs `<path> --version` and returns the semver it reports.
// A binary that will not run, or whose output carries no version, yields "" —
// unverifiable is NOT a pass, matching internal/leyline's own posture.
func leylineSemver(t *testing.T, env []string, path string) string {
	t.Helper()
	res := runner{env: env}.run(t, path, "--version")
	if res.err != nil || res.code != 0 {
		return ""
	}
	return firstSemver(res.combined())
}

// checkLeylinePin is the gate's actual comparator, factored out so its ability
// to REJECT can be exercised directly (see
// TestLeylinePinComparatorRejectsAStaleBinary). Returns nil only when path
// reports exactly want.
func checkLeylinePin(t *testing.T, env []string, path, want string) error {
	t.Helper()
	got := leylineSemver(t, env, path)
	if got == "" {
		return fmt.Errorf("%s reports no parseable version — refusing to treat an unverifiable leyline as the pin %s", path, want)
	}
	if got != want {
		return fmt.Errorf("leyline at %s reports %s, but mache pins %s", path, got, want)
	}
	return nil
}

// plantStaleLeyline writes an executable stub that answers --version with
// shadowVersion and shouts on stdout for any other argv, and returns its
// directory and path.
func plantStaleLeyline(t *testing.T) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "leyline")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--version\" ]; then echo 'leyline " + shadowVersion + " (open)'; exit 0; fi\n" +
		"echo SHADOW-RAN\n"
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return dir, path
}

// envWithPathPrefix returns the ambient environment with dir PREPENDED to PATH
// and MACHE_LEYLINE_BINARY cleared, so resolution is decided by PATH order and
// the pinned cache alone — no override short-circuits the question.
func envWithPathPrefix(t *testing.T, dir string) []string {
	t.Helper()
	out := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		switch {
		case strings.HasPrefix(kv, "PATH="):
			out = append(out, "PATH="+dir+string(os.PathListSeparator)+strings.TrimPrefix(kv, "PATH="))
		case strings.HasPrefix(kv, leyline.BinaryOverrideEnv+"="):
			// dropped
		default:
			out = append(out, kv)
		}
	}
	return out
}

// resolvedLeyline asks the INSTALLED mache which leyline it would use.
// `mache leyline path` prints the absolute path on stdout and nothing else
// (diagnostics go to stderr), which is the only reason this question is
// answerable from outside the process at all.
func resolvedLeyline(t *testing.T, bin string, env []string) string {
	t.Helper()
	res := runner{env: env}.run(t, bin, "leyline", "path")
	require.NoError(t, res.err, "run %s leyline path", bin)
	require.Zerof(t, res.code,
		"`%s leyline path` exited %d — an installed mache must be able to name the leyline it uses "+
			"(this subcommand is how the version question is asked; a binary without it predates PR #584)\n"+
			"--- stdout ---\n%s\n--- stderr ---\n%s", bin, res.code, res.stdout, res.stderr)
	p := strings.TrimSpace(res.stdout)
	require.NotEmpty(t, p, "`leyline path` printed nothing on stdout")
	require.True(t, filepath.IsAbs(p), "`leyline path` must print an absolute path, got %q", p)
	return p
}

// TestLeylineInUseIsThePin is gate (a) in its ambient form: whatever leyline
// the installed mache resolves in the environment you actually have must be
// the pinned one.
func TestLeylineInUseIsThePin(t *testing.T) {
	bin := macheBinary(t)
	want := expectedLeylinePin(t)

	got := resolvedLeyline(t, bin, nil)
	require.NoError(t, checkLeylinePin(t, nil, got, want),
		"the installed mache resolves a leyline that is not the pin — every _ast-derived "+
			"number it produces (projections, smell counts, node hashes) inherits that skew")
	t.Logf("resolved leyline: %s (%s), pin %s", got, leylineSemver(t, nil, got), want)
}

// TestStaleLeylineOnPathDoesNotShadowThePin is the scenario this gate exists
// for: bead mache-19326d, reproduced against the INSTALLED binary.
//
// A stale leyline is planted FIRST on PATH — proven live by asserting that a
// shell resolves it — and mache must still reach the pin. Before `mache
// leyline path` existed, PATH was the only answer a caller had, so this
// fixture is exactly the world the reporter was in; any regression that hands
// resolution back to PATH resolves the stub and turns this red.
func TestStaleLeylineOnPathDoesNotShadowThePin(t *testing.T) {
	bin := macheBinary(t)
	want := expectedLeylinePin(t)

	shadowDir, shadow := plantStaleLeyline(t)
	env := envWithPathPrefix(t, shadowDir)

	// Fixture liveness. Without this the test could pass because the shadow
	// was never reachable, which proves nothing.
	viaShell := runner{env: env}.mustRun(t, "/bin/sh", "-c", "command -v leyline")
	require.Equal(t, shadow, strings.TrimSpace(viaShell.stdout),
		"the fixture must actually shadow — a shell has to resolve the stale stub for this test to mean anything")
	require.NotEqual(t, want, shadowVersion, "the shadow must differ from the pin, or it shadows nothing")

	got := resolvedLeyline(t, bin, env)
	assert.NotEqual(t, shadow, got, "resolving the PATH copy is precisely the mache-19326d skew")
	assert.NoError(t, checkLeylinePin(t, env, got, want))

	// The exec surface must agree with the path surface — a caller who runs
	// `mache leyline exec` rather than interpolating `$(mache leyline path)`
	// must get the same binary.
	execd := runner{env: env}.mustRun(t, bin, "leyline", "exec", "--", "--version")
	assert.NotContains(t, execd.combined(), "SHADOW-RAN",
		"`mache leyline exec` must not proxy to the stale PATH copy")
	ran := firstSemver(execd.stdout)
	require.NotEmpty(t, ran, "`mache leyline exec -- --version` printed no version:\n%s", execd.combined())
	assert.Equal(t, want, ran, "the leyline mache RUNS must report the pin")
}

// TestLeylinePinComparatorRejectsAStaleBinary proves the comparator above can
// fail. A gate that cannot go red is worse than no gate: it reads as evidence
// while asserting nothing. Here the stale stub is fed to checkLeylinePin
// directly, and the rejection must name both versions so the failure is
// actionable rather than merely loud.
func TestLeylinePinComparatorRejectsAStaleBinary(t *testing.T) {
	want := expectedLeylinePin(t)
	_, shadow := plantStaleLeyline(t)

	err := checkLeylinePin(t, nil, shadow, want)
	require.Error(t, err, "a %s binary must not satisfy a %s pin", shadowVersion, want)
	assert.Contains(t, err.Error(), shadowVersion)
	assert.Contains(t, err.Error(), want)

	// An unverifiable binary is also a rejection, not a pass.
	unrunnable := filepath.Join(t.TempDir(), "leyline")
	require.NoError(t, os.WriteFile(unrunnable, []byte("not a program"), 0o644))
	assert.Error(t, checkLeylinePin(t, nil, unrunnable, want),
		"a leyline whose version cannot be read must be refused, not assumed to match")
}

// verifyHome is a HOME the gate owns. It is deliberately NOT t.TempDir():
// provisioning downloads a ~30MB release asset, and a per-run temp HOME would
// re-download on every `task check`. A stable location is genuinely clean the
// first time (and on every CI runner) and cheap afterwards, while the
// post-condition asserted below stays meaningful either way — see its caller.
func verifyHome(t *testing.T) string {
	t.Helper()
	home := filepath.Join(os.TempDir(), "mache-installverify-home")
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".mache", "bin"), 0o755))
	// Remove the UNVERSIONED path if a previous run (or anything else) left
	// one, so "absent afterwards" means mache did not write it rather than
	// "it was never there and nothing tried".
	require.NoError(t, os.RemoveAll(filepath.Join(home, ".mache", "bin", "leyline")))
	return home
}

// TestCleanHomeProvisionsOnlyTheVersionedPath is bead mache-19326d's
// acceptance criterion minus the container: from a HOME with no ~/.mache and a
// PATH with no leyline on it, the installed binary must provision the pin by
// itself — no repo, no Taskfile, no `task leyline:ensure`.
//
// It simultaneously guards mache-0acdf6 from the OUTSIDE: the unversioned
// ~/.mache/bin/leyline was a shared mutable resource concurrent mache builds
// overwrote, silently changing _ast shape between runs, so provisioning must
// write ONLY the version-namespaced path. cmd/leyline_proxy_test.go pins that
// in-process; this pins that the SHIPPED binary behaves the same way.
func TestCleanHomeProvisionsOnlyTheVersionedPath(t *testing.T) {
	bin := macheBinary(t)
	if os.Getenv("MACHE_NO_LEYLINE") != "" {
		t.Skip("MACHE_NO_LEYLINE set — provisioning is the subject of this test, so there is nothing to assert")
	}
	want := expectedLeylinePin(t)
	home := verifyHome(t)

	// PATH without any leyline, so the pinned cache is the only way to
	// succeed. /usr/bin:/bin keeps the download path's own shell-outs working.
	env := append(scrubbedEnv(t), "HOME="+home, "PATH=/usr/bin:/bin")

	got := resolvedLeyline(t, bin, env)
	assert.Equal(t, filepath.Join(home, ".mache", "bin", "leyline-"+leylinePinTag(t)), got,
		"provisioning must land in the VERSION-NAMESPACED cache path")
	assert.NoError(t, checkLeylinePin(t, env, got, want))

	_, err := os.Lstat(filepath.Join(home, ".mache", "bin", "leyline"))
	assert.Truef(t, os.IsNotExist(err),
		"the unversioned ~/.mache/bin/leyline must stay unwritten (mache-0acdf6); got %v", err)
}

// leylinePinTag is the pin in its v-prefixed tag form, which is how the cache
// namespaces filenames.
func leylinePinTag(t *testing.T) string {
	t.Helper()
	return "v" + expectedLeylinePin(t)
}

// scrubbedEnv is the ambient environment minus every variable that could
// pre-decide leyline resolution or relocate HOME.
func scrubbedEnv(t *testing.T) []string {
	t.Helper()
	drop := []string{"HOME=", "PATH=", leyline.BinaryOverrideEnv + "="}
	out := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		skip := false
		for _, p := range drop {
			if strings.HasPrefix(kv, p) {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, kv)
		}
	}
	return out
}
