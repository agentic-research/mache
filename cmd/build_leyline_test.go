package cmd

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/internal/leyline"
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

// TestBuild_BackendFlagDefaultIsAuto pins the cobra wiring for the
// post-ADR-0012-step-4 contract: `--backend` still EXISTS (so existing
// invocations like `--backend=tree-sitter` don't break) and still
// defaults to "auto", but it is now a documented no-op — ley-line is
// the sole parser, and the usage text must say the flag is deprecated
// and ignored so scripts passing it get an honest signal from --help.
func TestBuild_BackendFlagDefaultIsAuto(t *testing.T) {
	flag := buildCmd.Flags().Lookup("backend")
	require.NotNil(t, flag, "buildCmd must keep registering --backend for backward compatibility")
	assert.Equal(t, "auto", flag.DefValue, "default must stay 'auto' so old invocations parse unchanged")
	assert.Contains(t, flag.Usage, "Deprecated", "help text must mark the flag deprecated")
	assert.Contains(t, flag.Usage, "ignored", "help text must say the value is ignored")
	assert.Contains(t, flag.Usage, "ley-line", "help text must say ley-line is the sole parser now")
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
	// Opt out of the build-path auto-download (mache-0ed19b) so "leyline
	// missing" surfaces as a clear error instead of a network fetch.
	t.Setenv("MACHE_NO_LEYLINE", "1")

	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "main.go"),
		[]byte("package main\nfunc main() {}\n"), 0o644))

	output := filepath.Join(t.TempDir(), "out.db")
	err := runBuildViaLeyline(src, output, true)
	require.Error(t, err, "leyline missing must error, not silently fall back")
	assert.Contains(t, err.Error(), "leyline backend",
		"error must identify which backend failed (helps the user diagnose)")
}

// TestRunBuildViaLeyline_HappyPath skips when leyline isn't
// available. When it is, parses a tiny Go tree and asserts the
// output .db exists with non-zero size at the user-supplied path
// (not the temp path autoInvokeLeylineParse uses internally).
// requirePinnedLeyline skips the test unless the exact-pinned leyline
// is already resolvable WITHOUT a network download (PATH or the
// ~/.mache/bin cache). LookPath alone is the wrong gate since the
// exact-version pin (mache-608a3c): a stale PATH leyline no longer
// satisfies the pin, and we don't want tests fetching from GitHub.
func requirePinnedLeyline(t *testing.T) {
	t.Helper()
	if _, err := leyline.ResolveBinary(false); err != nil {
		t.Skipf("pinned leyline not available without download: %v", err)
	}
}

func TestRunBuildViaLeyline_HappyPath(t *testing.T) {
	requirePinnedLeyline(t)
	saved := saveBuildFlags()
	defer saved.restore()

	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "main.go"),
		[]byte("package main\nfunc main() {}\n"), 0o644))

	output := filepath.Join(t.TempDir(), "out.db")
	require.NoError(t, runBuildViaLeyline(src, output, true))

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
	// Opt out of the build-path auto-download (mache-0ed19b) so "leyline
	// missing" surfaces as a clear error instead of a network fetch.
	t.Setenv("MACHE_NO_LEYLINE", "1")

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

// TestRunBuildViaLeyline_SchemaLoadErrorSurfaces pins the new schema
// branch (mache-73b885): --schema on the leyline path is now HONORED,
// so a broken schema path must surface as a load error BEFORE any
// shell-out to leyline (no binary needed for this test). This replaces
// the retired "explicit backend + schema is a contradiction" error.
func TestRunBuildViaLeyline_SchemaLoadErrorSurfaces(t *testing.T) {
	saved := saveBuildFlags()
	defer saved.restore()

	schemaPath = filepath.Join(t.TempDir(), "does-not-exist.json")

	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "main.go"),
		[]byte("package main\nfunc main() {}\n"), 0o644))

	output := filepath.Join(t.TempDir(), "out.db")
	err := runBuildViaLeyline(src, output, true)
	require.Error(t, err, "a missing schema file must fail the build")
	assert.Contains(t, err.Error(), "load schema",
		"error must come from the schema-load step, not a backend contradiction")
}

// TestRunBuildViaLeyline_SchemaProceedsToParse pins that a VALID
// schema no longer errors or gets dropped on the leyline path: with
// the binary hidden, the failure must be leyline-missing (i.e. we got
// PAST schema loading and into the parse step), not any schema-flag
// complaint. Replaces the retired warn-and-ignore behavior test.
func TestRunBuildViaLeyline_SchemaProceedsToParse(t *testing.T) {
	saved := saveBuildFlags()
	defer saved.restore()

	// Minimal but VALID topology file.
	schemaFile := filepath.Join(t.TempDir(), "schema.json")
	require.NoError(t, os.WriteFile(schemaFile,
		[]byte(`{"version":"v1alpha1"}`), 0o644))
	schemaPath = schemaFile

	// Hide leyline so the parse step fails deterministically.
	t.Setenv("PATH", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MACHE_NO_LEYLINE", "1")

	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "main.go"),
		[]byte("package main\nfunc main() {}\n"), 0o644))

	output := filepath.Join(t.TempDir(), "out.db")
	err := runBuildViaLeyline(src, output, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "leyline backend",
		"a valid schema must proceed to the leyline parse step")
	assert.NotContains(t, err.Error(), "does not honor --schema",
		"the schema-contradiction error is retired — schemas are honored now")
}

// TestRunBuildViaLeylineSchema_ProducesSchemaShapedDB is the
// end-to-end pin for schema-on-leyline (mache-73b885): building with
// the Go example schema through the leyline backend must produce a
// SCHEMA-shaped nodes table (construct-category dirs like
// 'functions/'), i.e. the Engine+ASTWalker projection ran — not
// leyline's own node layout, and not an ignored schema. Engine-level
// byte-parity with the SitterWalker projection is separately pinned
// by internal/ingest/ast_parity_test.go.
func TestRunBuildViaLeylineSchema_ProducesSchemaShapedDB(t *testing.T) {
	requirePinnedLeyline(t)
	saved := saveBuildFlags()
	defer saved.restore()

	// resolveSchema containment-checks relative paths against the
	// process cwd, so stage the example schema inside a temp cwd
	// rather than reaching out of the package dir with "..".
	schemaBytes, err := os.ReadFile(filepath.Join("..", "examples", "go-schema.json"))
	require.NoError(t, err)
	work := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(work, "schema.json"), schemaBytes, 0o644))
	t.Chdir(work)
	schemaPath = "schema.json"

	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "main.go"),
		[]byte("package main\n\nfunc Exported() int { return 1 }\n\nfunc main() {}\n"), 0o644))

	output := filepath.Join(t.TempDir(), "out.db")
	require.NoError(t, runBuildViaLeyline(src, output, true))

	db, err := sql.Open("sqlite", output)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	var n int
	require.NoError(t, db.QueryRow(
		"SELECT COUNT(*) FROM nodes WHERE id LIKE '%functions/%'").Scan(&n))
	assert.Positive(t, n,
		"schema projection must produce go-schema construct dirs (functions/)")

	var backend string
	require.NoError(t, db.QueryRow(
		"SELECT value FROM _mache_meta WHERE key = 'backend'").Scan(&backend))
	assert.Equal(t, "leyline+schema", backend,
		"metadata must record the schema-projected leyline backend")
}

// silence unused-import warning when tests don't reference cobra.
var _ = cobra.Command{}
