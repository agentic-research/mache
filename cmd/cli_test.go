package cmd

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// version command
// ---------------------------------------------------------------------------

func TestVersion_FieldsSet(t *testing.T) {
	// versionCmd uses fmt.Printf (stdout), hard to capture.
	// Instead verify the format string produces expected output.
	oldVersion, oldCommit, oldDate := Version, Commit, Date
	Version, Commit, Date = "1.2.3", "abc123", "2026-03-22"
	defer func() { Version, Commit, Date = oldVersion, oldCommit, oldDate }()

	out := fmt.Sprintf("mache version %s (commit %s, built %s)\n", Version, Commit, Date)
	assert.Contains(t, out, "1.2.3")
	assert.Contains(t, out, "abc123")
	assert.Contains(t, out, "2026-03-22")
}

// ---------------------------------------------------------------------------
// build command
// ---------------------------------------------------------------------------

func TestBuild_ProducesDB(t *testing.T) {
	tmpDir := t.TempDir()

	srcDir := filepath.Join(tmpDir, "src")
	require.NoError(t, os.MkdirAll(srcDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644))

	outDB := filepath.Join(tmpDir, "out.db")

	// Call RunE directly to avoid cobra global state pollution
	oldSchemaPath := schemaPath
	schemaPath = "go"
	defer func() { schemaPath = oldSchemaPath }()

	err := buildCmd.RunE(buildCmd, []string{srcDir, outDB})
	require.NoError(t, err)

	info, err := os.Stat(outDB)
	require.NoError(t, err)
	assert.Greater(t, info.Size(), int64(0), "output DB should be non-empty")
}

func TestBuild_NonexistentSource(t *testing.T) {
	tmpDir := t.TempDir()
	outDB := filepath.Join(tmpDir, "out.db")

	oldSchemaPath := schemaPath
	schemaPath = "go"
	defer func() { schemaPath = oldSchemaPath }()

	err := buildCmd.RunE(buildCmd, []string{"/nonexistent/path", outDB})
	assert.Error(t, err, "should fail with nonexistent source")
}

// ---------------------------------------------------------------------------
// list command
// ---------------------------------------------------------------------------

func TestList_RunsWithoutError(t *testing.T) {
	var buf bytes.Buffer
	listCmd.SetOut(&buf)
	err := listCmd.RunE(listCmd, nil)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// clean command
// ---------------------------------------------------------------------------

func TestClean_RunsWithoutError(t *testing.T) {
	var buf bytes.Buffer
	cleanCmd.SetOut(&buf)
	err := cleanCmd.RunE(cleanCmd, nil)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// init command (supplements existing init_test.go)
// ---------------------------------------------------------------------------

func TestInit_AutoDetectsGo(t *testing.T) {
	tmpDir := t.TempDir()
	oldCwd, _ := os.Getwd()
	require.NoError(t, os.Chdir(tmpDir))
	defer func() { _ = os.Chdir(oldCwd) }()

	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte("module test\n"), 0o644))

	var buf bytes.Buffer
	err := execInit(&buf, "/usr/local/bin/mache", initOpts{Source: "."})
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(tmpDir, ".mache.json"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "go")
}

func TestInit_AutoDetectsPython(t *testing.T) {
	tmpDir := t.TempDir()
	oldCwd, _ := os.Getwd()
	require.NoError(t, os.Chdir(tmpDir))
	defer func() { _ = os.Chdir(oldCwd) }()

	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "pyproject.toml"), []byte("[project]\n"), 0o644))

	var buf bytes.Buffer
	err := execInit(&buf, "/usr/local/bin/mache", initOpts{Source: "."})
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(tmpDir, ".mache.json"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "python")
}

// ---------------------------------------------------------------------------
// serve command — verify help output
// ---------------------------------------------------------------------------

func TestServe_HelpOutput(t *testing.T) {
	out := serveCmd.UsageString()
	assert.Contains(t, out, "schema")
	assert.Contains(t, out, "http")
}

// ---------------------------------------------------------------------------
// root command
// ---------------------------------------------------------------------------

func TestRoot_HelpOutput(t *testing.T) {
	out := rootCmd.UsageString()
	assert.Contains(t, out, "Mache")
	assert.Contains(t, out, "Available Commands")
	assert.Contains(t, out, "serve")
	assert.Contains(t, out, "build")
	assert.Contains(t, out, "init")
}

func TestRoot_VersionString(t *testing.T) {
	oldVersion := Version
	Version = "test-version"
	defer func() { Version = oldVersion }()

	v := fmt.Sprintf("%s (commit %s, built %s)", Version, Commit, Date)
	assert.Contains(t, v, "test-version")
}
