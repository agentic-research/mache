package cmd

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentic-research/mache/internal/leyline"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pinnedLeylineSemver is the pin without its leading "v" — the form a real
// leyline prints in `leyline <ver> (open)`.
func pinnedLeylineSemver() string { return strings.TrimPrefix(leyline.BinaryVersion, "v") }

// writeLeylineStub plants an executable fake leyline at path. It answers
// `--version` with "leyline <version> (open)" (the shape internal/leyline's
// version gate parses) and runs body for every other argv.
//
// body must use only shell BUILTINS: isolateLeylineResolution empties PATH so
// that resolution is decided solely by planted binaries, which also means the
// stub inherits a PATH with no /bin on it (an earlier draft used `cat` and got
// "command not found").
func writeLeylineStub(t *testing.T, path, version, body string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--version\" ]; then echo 'leyline " + version + " (open)'; exit 0; fi\n" +
		body + "\n"
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

// isolateLeylineResolution makes leyline resolution depend ONLY on what the
// test plants: an empty HOME (so no real ~/.mache cache leaks in), a PATH
// containing exactly one scratch dir, no MACHE_LEYLINE_BINARY override, and
// MACHE_NO_LEYLINE set so a resolution miss fails instead of hitting the
// network. Returns (home, pathDir).
func isolateLeylineResolution(t *testing.T) (string, string) {
	t.Helper()
	home, pathDir := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", pathDir)
	t.Setenv(leyline.BinaryOverrideEnv, "")
	t.Setenv("MACHE_NO_LEYLINE", "1")
	return home, pathDir
}

// pinnedCacheStubPath is where mache's version-namespaced cache keeps THIS
// build's pin. Note it is never the unversioned ~/.mache/bin/leyline, which
// mache-0acdf6 made read-only-never-written.
func pinnedCacheStubPath(home string) string {
	return filepath.Join(home, ".mache", "bin", "leyline-"+leyline.BinaryVersion)
}

// runLeylinePath invokes `mache leyline path` in-process and returns stdout.
func runLeylinePath(t *testing.T) (string, error) {
	t.Helper()
	var out bytes.Buffer
	leylinePathCmd.SetOut(&out)
	t.Cleanup(func() { leylinePathCmd.SetOut(nil) })
	err := leylinePathCmd.RunE(leylinePathCmd, nil)
	return strings.TrimSpace(out.String()), err
}

// ---------------------------------------------------------------------------
// The bug this exists to fix: a stale leyline on PATH shadowing the pin.
// ---------------------------------------------------------------------------

// TestLeylinePath_ResolvesPinnedDespiteStalePATHShadow reproduces the exact
// three-way skew measured in mache-19326d — a 0.10.3 leyline first on PATH
// while the pinned build sits in a cache dir that is not on PATH — and asserts
// `mache leyline path` reports the PIN, not the shadow.
//
// The LookPath assertion in the middle is the falsifier: it pins down that a
// shell (i.e. a human or agent typing bare `leyline`) genuinely resolves the
// stale binary in this fixture. Any implementation that answers this question
// with PATH — the pre-fix state of the world, where PATH was the only answer a
// caller had — resolves to that same stale path and fails the assertions below.
func TestLeylinePath_ResolvesPinnedDespiteStalePATHShadow(t *testing.T) {
	home, pathDir := isolateLeylineResolution(t)

	shadow := writeLeylineStub(t, filepath.Join(pathDir, "leyline"), "0.10.3", "echo SHADOW-RAN")
	pinned := writeLeylineStub(t, pinnedCacheStubPath(home), pinnedLeylineSemver(), "echo PINNED-RAN")

	viaPATH, err := exec.LookPath("leyline")
	require.NoError(t, err)
	require.Equal(t, shadow, viaPATH,
		"fixture must actually shadow — PATH has to resolve the stale stub for this test to mean anything")

	got, err := runLeylinePath(t)
	require.NoError(t, err)

	assert.Equal(t, pinned, got, "must report the pinned binary from the version-namespaced cache")
	assert.NotEqual(t, viaPATH, got, "reporting the PATH copy is precisely the mache-19326d version skew")
	assert.True(t, filepath.IsAbs(got), "callers substitute this into a command line from arbitrary cwds")
}

// TestLeylineExec_RunsPinnedDespiteStalePATHShadow is the same shadowing proof
// for the exec surface: the process that actually runs must be the pin.
func TestLeylineExec_RunsPinnedDespiteStalePATHShadow(t *testing.T) {
	home, pathDir := isolateLeylineResolution(t)

	writeLeylineStub(t, filepath.Join(pathDir, "leyline"), "0.10.3", "echo SHADOW-RAN")
	writeLeylineStub(t, pinnedCacheStubPath(home), pinnedLeylineSemver(), "echo PINNED-RAN")

	var stdout, stderr bytes.Buffer
	code, err := runLeylineExec([]string{"--", "cdc", "enable"}, strings.NewReader(""), &stdout, &stderr)
	require.NoError(t, err)
	assert.Zero(t, code)
	assert.Contains(t, stdout.String(), "PINNED-RAN")
	assert.NotContains(t, stdout.String(), "SHADOW-RAN", "exec must not proxy to the stale PATH copy")
}

// ---------------------------------------------------------------------------
// Resolution tiers
// ---------------------------------------------------------------------------

func TestLeylinePath_ResolutionTiers(t *testing.T) {
	tests := []struct {
		name        string
		pathVersion string // planted on PATH; "" = nothing on PATH
		cached      bool   // plant the pinned binary in the namespaced cache
		wantTier    string // "path" | "cache"
		wantErrHas  string
	}{
		{
			name:        "pinned on PATH is accepted",
			pathVersion: pinnedLeylineSemver(),
			wantTier:    "path",
		},
		{
			name:        "stale on PATH loses to the cached pin",
			pathVersion: "0.10.3",
			cached:      true,
			wantTier:    "cache",
		},
		{
			name:     "cache alone resolves when PATH is empty",
			cached:   true,
			wantTier: "cache",
		},
		{
			// A newer-than-pin PATH build is still a different _ast producer.
			name:        "newer-than-pin on PATH is rejected, not preferred",
			pathVersion: "99.0.0",
			cached:      true,
			wantTier:    "cache",
		},
		{
			name:        "stale on PATH with no cache errors naming the pin",
			pathVersion: "0.10.3",
			wantErrHas:  leyline.BinaryVersion,
		},
		{
			name:        "a binary with no parseable version is not trusted",
			pathVersion: "not-a-version",
			wantErrHas:  leyline.BinaryVersion,
		},
		{
			name:       "nothing anywhere errors naming the pin",
			wantErrHas: leyline.BinaryVersion,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home, pathDir := isolateLeylineResolution(t)

			var onPath string
			if tt.pathVersion != "" {
				onPath = writeLeylineStub(t, filepath.Join(pathDir, "leyline"), tt.pathVersion, "exit 0")
			}
			var inCache string
			if tt.cached {
				inCache = writeLeylineStub(t, pinnedCacheStubPath(home), pinnedLeylineSemver(), "exit 0")
			}

			got, err := runLeylinePath(t)

			if tt.wantErrHas != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErrHas,
					"resolution failures must reuse ResolveBinary's wording, which names the pin")
				assert.Empty(t, got, "no path may be printed when resolution failed")
				return
			}
			require.NoError(t, err)
			switch tt.wantTier {
			case "path":
				assert.Equal(t, onPath, got)
			case "cache":
				assert.Equal(t, inCache, got)
			}
		})
	}
}

// TestLeylinePath_NeverWritesUnversionedCachePath guards mache-0acdf6: the
// unversioned ~/.mache/bin/leyline is a shared mutable resource concurrent
// mache builds used to fight over, so it is read-only-never-written. Resolving
// through this command must not change that.
func TestLeylinePath_NeverWritesUnversionedCachePath(t *testing.T) {
	home, _ := isolateLeylineResolution(t)
	writeLeylineStub(t, pinnedCacheStubPath(home), pinnedLeylineSemver(), "exit 0")

	got, err := runLeylinePath(t)
	require.NoError(t, err)
	assert.Equal(t, pinnedCacheStubPath(home), got)

	unversioned := filepath.Join(home, ".mache", "bin", "leyline")
	_, statErr := os.Lstat(unversioned)
	assert.True(t, os.IsNotExist(statErr),
		"the unversioned path must stay unwritten (mache-0acdf6); got %v", statErr)
}

// ---------------------------------------------------------------------------
// exec proxy fidelity
// ---------------------------------------------------------------------------

func TestLeylineExec_ProxyFidelity(t *testing.T) {
	// The stub reports what it received so the proxy contract is observable:
	// argv on stdout, a marker on stderr, stdin echoed back, exit status taken
	// from the first argument when it is a bare number.
	const stubBody = `echo "argv:$*"
echo "stub-stderr" >&2
IFS= read -r line
echo "stdin:$line"
case "$1" in
  exit-*) exit "${1#exit-}" ;;
esac
exit 0`

	tests := []struct {
		name       string
		args       []string
		stdin      string
		wantArgv   string
		wantCode   int
		wantStdout []string
	}{
		{
			name:       "double dash separator is stripped before leyline sees it",
			args:       []string{"--", "cdc", "enable", "--db", "x.db"},
			wantArgv:   "argv:cdc enable --db x.db",
			wantStdout: []string{"argv:cdc enable --db x.db"},
		},
		{
			name:       "args pass through without a separator",
			args:       []string{"cdc", "gc", "--db", "x.db"},
			wantArgv:   "argv:cdc gc --db x.db",
			wantStdout: []string{"argv:cdc gc --db x.db"},
		},
		{
			name:       "leyline flags are not claimed by mache",
			args:       []string{"--help"},
			wantArgv:   "argv:--help",
			wantStdout: []string{"argv:--help"},
		},
		{
			name:       "a later double dash belongs to leyline",
			args:       []string{"--", "parse", "--", "extra"},
			wantArgv:   "argv:parse -- extra",
			wantStdout: []string{"argv:parse -- extra"},
		},
		{
			name:       "no args runs leyline bare",
			args:       nil,
			wantArgv:   "argv:",
			wantStdout: []string{"argv:"},
		},
		{
			name:       "stdin is forwarded",
			args:       []string{"parse"},
			stdin:      "from-caller\n",
			wantStdout: []string{"stdin:from-caller"},
		},
		{
			name:       "non-zero exit status is reported, not flattened",
			args:       []string{"exit-7"},
			wantCode:   7,
			wantStdout: []string{"argv:exit-7"},
		},
		{
			name:       "exit status 1 is still distinguishable from a launch failure",
			args:       []string{"exit-1"},
			wantCode:   1,
			wantStdout: []string{"argv:exit-1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home, _ := isolateLeylineResolution(t)
			writeLeylineStub(t, pinnedCacheStubPath(home), pinnedLeylineSemver(), stubBody)

			var stdout, stderr bytes.Buffer
			code, err := runLeylineExec(tt.args, strings.NewReader(tt.stdin), &stdout, &stderr)

			require.NoError(t, err, "leyline ran, so err is reserved for resolve/launch failures")
			assert.Equal(t, tt.wantCode, code)
			for _, want := range tt.wantStdout {
				assert.Contains(t, stdout.String(), want)
			}
			assert.Contains(t, stderr.String(), "stub-stderr", "leyline's stderr must stay on stderr")
			assert.NotContains(t, stdout.String(), "stub-stderr", "stderr must not be folded into stdout")
		})
	}
}

// TestLeylineExec_ResolutionFailureIsAnError separates the two failure modes:
// "leyline ran and said no" (exit code) versus "there is no pinned leyline"
// (error). Only the latter should surface ResolveBinary's message.
func TestLeylineExec_ResolutionFailureIsAnError(t *testing.T) {
	isolateLeylineResolution(t) // nothing planted anywhere

	var stdout, stderr bytes.Buffer
	code, err := runLeylineExec([]string{"cdc", "gc"}, strings.NewReader(""), &stdout, &stderr)

	require.Error(t, err)
	assert.Contains(t, err.Error(), leyline.BinaryVersion, "must name the pin, reusing ResolveBinary's wording")
	assert.Zero(t, code)
	assert.Empty(t, stdout.String())
}

// TestLeylineExecCmd_PropagatesExitStatus covers the cobra wiring: a non-zero
// leyline status must reach os.Exit rather than being returned as an error,
// which Execute() would flatten to 1.
func TestLeylineExecCmd_PropagatesExitStatus(t *testing.T) {
	home, _ := isolateLeylineResolution(t)
	writeLeylineStub(t, pinnedCacheStubPath(home), pinnedLeylineSemver(), "exit 42")

	var gotCode int
	exited := false
	orig := leylineExecExit
	leylineExecExit = func(code int) { gotCode, exited = code, true }
	t.Cleanup(func() { leylineExecExit = orig })

	leylineExecCmd.SetOut(&bytes.Buffer{})
	leylineExecCmd.SetErr(&bytes.Buffer{})
	t.Cleanup(func() { leylineExecCmd.SetOut(nil); leylineExecCmd.SetErr(nil) })

	require.NoError(t, leylineExecCmd.RunE(leylineExecCmd, []string{"--", "cdc", "gc"}))
	assert.True(t, exited, "a non-zero leyline status must terminate mache with that status")
	assert.Equal(t, 42, gotCode)
}

// TestLeylineExecCmd_ZeroStatusDoesNotExit — the success path must fall through
// normally so deferred cleanup elsewhere in mache still runs.
func TestLeylineExecCmd_ZeroStatusDoesNotExit(t *testing.T) {
	home, _ := isolateLeylineResolution(t)
	writeLeylineStub(t, pinnedCacheStubPath(home), pinnedLeylineSemver(), "exit 0")

	orig := leylineExecExit
	called := false
	leylineExecExit = func(int) { called = true }
	t.Cleanup(func() { leylineExecExit = orig })

	leylineExecCmd.SetOut(&bytes.Buffer{})
	leylineExecCmd.SetErr(&bytes.Buffer{})
	t.Cleanup(func() { leylineExecCmd.SetOut(nil); leylineExecCmd.SetErr(nil) })

	require.NoError(t, leylineExecCmd.RunE(leylineExecCmd, []string{"--", "parse"}))
	assert.False(t, called, "a zero status must not call os.Exit")
}

// ---------------------------------------------------------------------------
// Command surface
// ---------------------------------------------------------------------------

// TestLeylineCmd_Registered guards the surface itself: these subcommands only
// help if they are reachable as `mache leyline path` / `mache leyline exec`.
func TestLeylineCmd_Registered(t *testing.T) {
	var parent *cobra.Command
	for _, c := range rootCmd.Commands() {
		if c.Name() == "leyline" {
			parent = c
			break
		}
	}
	require.NotNil(t, parent, "`mache leyline` must be registered on the root command")

	subs := map[string]bool{}
	for _, c := range parent.Commands() {
		subs[c.Name()] = true
	}
	assert.True(t, subs["path"], "`mache leyline path` must exist")
	assert.True(t, subs["exec"], "`mache leyline exec` must exist")

	assert.True(t, leylineExecCmd.DisableFlagParsing,
		"exec must not parse flags, or leyline's own flags never reach leyline")
}
