package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// saveBuildFlags / restoreBuildFlags isolate package-level flag state
// across tests. Cobra binds buildBackend / schemaPath as package vars
// so tests that toggle them must restore.
type buildFlagSnapshot struct {
	backend, schema string
}

func saveBuildFlags() buildFlagSnapshot {
	return buildFlagSnapshot{backend: buildBackend, schema: schemaPath}
}

func (s buildFlagSnapshot) restore() {
	buildBackend = s.backend
	schemaPath = s.schema
}

// TestBuild_BackendFlagDefaultIsAuto pins the cobra wiring: the
// `--backend` flag exists on buildCmd and defaults to "auto".
// Important to lock down because `auto` preserves today's
// behavior; flipping the default to `leyline` is a separate
// future change that should land deliberately.
func TestBuild_BackendFlagDefaultIsAuto(t *testing.T) {
	flag := buildCmd.Flags().Lookup("backend")
	require.NotNil(t, flag, "buildCmd must register --backend")
	assert.Equal(t, "auto", flag.DefValue, "default backend must remain 'auto' until ADR-0012 commits to leyline-default")
	assert.Contains(t, flag.Usage, "leyline", "help text must mention the leyline option")
	assert.Contains(t, flag.Usage, "auto", "help text must call out the current default")
}

// TestRunBuildViaLeyline_ReturnsClearErrorWhenLeylineMissing pins
// the explicit-flag-no-fallback contract: --backend=leyline with
// no leyline binary on PATH (and no bundled location) must surface
// a clear error rather than silently fall back to in-process
// parsing. Silent fallback would mask misconfiguration in CI/script
// contexts where the user expects the leyline path.
func TestRunBuildViaLeyline_ReturnsClearErrorWhenLeylineMissing(t *testing.T) {
	saved := saveBuildFlags()
	defer saved.restore()

	// Hide both PATH and the bundled location ($HOME/.mache/bin/leyline).
	t.Setenv("PATH", "")
	t.Setenv("HOME", t.TempDir())

	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "main.go"),
		[]byte("package main\nfunc main() {}\n"), 0o644))

	output := filepath.Join(t.TempDir(), "out.db")
	err := runBuildViaLeyline(src, output)
	require.Error(t, err, "leyline missing must error, not silently fall back")
	assert.Contains(t, err.Error(), "leyline backend",
		"error must identify which backend failed (helps the user diagnose)")
}

// TestRunBuildViaLeyline_HappyPath skips when leyline isn't
// available. When it is, parses a tiny Go tree and asserts the
// output .db exists with non-zero size at the user-supplied path
// (not the temp path autoInvokeLeylineParse uses internally).
func TestRunBuildViaLeyline_HappyPath(t *testing.T) {
	if _, err := exec.LookPath("leyline"); err != nil {
		t.Skip("leyline binary not on PATH")
	}
	saved := saveBuildFlags()
	defer saved.restore()

	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "main.go"),
		[]byte("package main\nfunc main() {}\n"), 0o644))

	output := filepath.Join(t.TempDir(), "out.db")
	require.NoError(t, runBuildViaLeyline(src, output))

	info, err := os.Stat(output)
	require.NoError(t, err, "output .db must exist at the user-supplied path")
	assert.Greater(t, info.Size(), int64(0), "output .db must be non-empty")
}

// TestBuildCmd_BackendDispatch pins that the cobra command actually
// routes --backend=leyline through runBuildViaLeyline rather than
// the in-process engine. Uses the missing-leyline error as a
// detection signal: if the leyline path runs, we get a leyline-
// flavored error; if the auto path runs, we'd get a tree-sitter-
// flavored error or success.
func TestBuildCmd_BackendDispatch(t *testing.T) {
	saved := saveBuildFlags()
	defer saved.restore()

	// Force leyline backend, hide the binary.
	buildBackend = "leyline"
	t.Setenv("PATH", "")
	t.Setenv("HOME", t.TempDir())

	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "main.go"),
		[]byte("package main\nfunc main() {}\n"), 0o644))
	output := filepath.Join(t.TempDir(), "out.db")

	// Run the cobra command directly. ParseFlags + RunE is the same
	// pattern TestFindSmellsCLI uses (executes without walking up to
	// rootCmd which expects a mountpoint).
	require.NoError(t, buildCmd.ParseFlags([]string{}))
	err := buildCmd.RunE(buildCmd, []string{src, output})
	require.Error(t, err, "leyline path must error when binary is missing")
	assert.Contains(t, err.Error(), "leyline backend",
		"dispatch must reach runBuildViaLeyline, not the in-process engine")
}

// silence unused-import warning when tests don't reference cobra.
var _ = cobra.Command{}
