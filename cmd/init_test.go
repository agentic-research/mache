package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInit_CreatesFiles(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chdir(dir))

	// Create a Go file so auto-detect works
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644))

	buf := new(bytes.Buffer)
	err := execInit(buf, "mache", initOpts{Source: "."})
	require.NoError(t, err)

	// Check .mache.json
	data, err := os.ReadFile(filepath.Join(dir, ConfigFileName))
	require.NoError(t, err)

	var cfg ProjectConfig
	require.NoError(t, json.Unmarshal(data, &cfg))
	assert.Len(t, cfg.Sources, 1)
	assert.Equal(t, ".", cfg.Sources[0].Path)
	assert.Equal(t, "go", cfg.Sources[0].Schema)

	// Check .claude/mcp.json
	mcpData, err := os.ReadFile(filepath.Join(dir, ".claude", "mcp.json"))
	require.NoError(t, err)
	assert.Contains(t, string(mcpData), "mache")
	assert.Contains(t, string(mcpData), "serve")

	// Check .claude/CLAUDE.md
	claudeMD, err := os.ReadFile(filepath.Join(dir, ".claude", "CLAUDE.md"))
	require.NoError(t, err)
	assert.Contains(t, string(claudeMD), "## Mache")
	assert.Contains(t, string(claudeMD), "list_directory")
	assert.Contains(t, string(claudeMD), "find_callers")
	assert.Contains(t, string(claudeMD), "**go**")

	// Check output
	assert.Contains(t, buf.String(), "Created .mache.json")
	assert.Contains(t, buf.String(), ".claude/mcp.json")
	assert.Contains(t, buf.String(), "CLAUDE.md")
}

func TestInit_ExistingConfigNoForce(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chdir(dir))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ConfigFileName), []byte("{}"), 0o644))

	err := execInit(new(bytes.Buffer), "mache", initOpts{Source: "."})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
}

func TestInit_ExistingConfigWithForce(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chdir(dir))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ConfigFileName), []byte("{}"), 0o644))

	err := execInit(new(bytes.Buffer), "mache", initOpts{Force: true, Source: "."})
	require.NoError(t, err)
}

func TestInit_ExplicitSchema(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chdir(dir))

	err := execInit(new(bytes.Buffer), "mache", initOpts{Schema: "python", Source: "."})
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(dir, ConfigFileName))
	require.NoError(t, err)

	var cfg ProjectConfig
	require.NoError(t, json.Unmarshal(data, &cfg))
	assert.Equal(t, "python", cfg.Sources[0].Schema)
}

func TestInit_Global(t *testing.T) {
	// Override HOME so we don't touch real config
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// Mock Claude CLI to avoid real exec side effects
	orig := claudeCLIRegister
	claudeCLIRegister = func(string) bool { return false }
	t.Cleanup(func() { claudeCLIRegister = orig })

	// Create a fake .cursor dir so registerEditorMCP finds it
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".cursor"), 0o755))

	buf := new(bytes.Buffer)
	err := execInit(buf, "mache", initOpts{Global: true, Source: "."})
	require.NoError(t, err)

	// Check Cursor's mcp.json was created
	mcpData, err := os.ReadFile(filepath.Join(dir, ".cursor", "mcp.json"))
	require.NoError(t, err)
	assert.Contains(t, string(mcpData), "mache")
	assert.Contains(t, string(mcpData), "serve")

	// No .mache.json should be created in global mode
	_, err = os.Stat(filepath.Join(dir, ConfigFileName))
	assert.True(t, os.IsNotExist(err))

	assert.Contains(t, buf.String(), "Restart your editor")
}

func TestInit_CLAUDEmd_AppendToExisting(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chdir(dir))

	// Create existing CLAUDE.md with other content
	claudeDir := filepath.Join(dir, ".claude")
	require.NoError(t, os.MkdirAll(claudeDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(claudeDir, "CLAUDE.md"),
		[]byte("## Existing Section\n\nSome instructions.\n"),
		0o644,
	))

	err := execInit(new(bytes.Buffer), "mache", initOpts{Schema: "go", Source: "."})
	require.NoError(t, err)

	claudeMD, err := os.ReadFile(filepath.Join(claudeDir, "CLAUDE.md"))
	require.NoError(t, err)
	content := string(claudeMD)
	assert.Contains(t, content, "## Existing Section")
	assert.Contains(t, content, "## Mache")
}

func TestInit_CLAUDEmd_NoDuplicate(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chdir(dir))

	opts := initOpts{Force: true, Schema: "go", Source: "."}

	// Run init twice
	require.NoError(t, execInit(new(bytes.Buffer), "mache", opts))
	require.NoError(t, execInit(new(bytes.Buffer), "mache", opts))

	claudeMD, err := os.ReadFile(filepath.Join(dir, ".claude", "CLAUDE.md"))
	require.NoError(t, err)
	// Should only have one "## Mache" section
	assert.Equal(t, 1, strings.Count(string(claudeMD), "## Mache"))
}

func TestInit_CustomSource(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chdir(dir))

	err := execInit(new(bytes.Buffer), "mache", initOpts{Source: "./data/mydb.db"})
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(dir, ConfigFileName))
	require.NoError(t, err)

	var cfg ProjectConfig
	require.NoError(t, json.Unmarshal(data, &cfg))
	assert.Equal(t, "./data/mydb.db", cfg.Sources[0].Path)
}
