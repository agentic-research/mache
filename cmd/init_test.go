package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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

	// Reset flags for test isolation
	initForce = false
	initSchema = ""
	initSource = "."

	initCmd.SetOut(buf)
	err := runInit(initCmd, nil)
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

	// Check output
	assert.Contains(t, buf.String(), "Created .mache.json")
	assert.Contains(t, buf.String(), ".claude/mcp.json")
}

func TestInit_ExistingConfigNoForce(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chdir(dir))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ConfigFileName), []byte("{}"), 0o644))

	initForce = false
	initSchema = ""
	initSource = "."

	err := runInit(initCmd, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
}

func TestInit_ExistingConfigWithForce(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chdir(dir))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ConfigFileName), []byte("{}"), 0o644))

	buf := new(bytes.Buffer)
	initCmd.SetOut(buf)

	initForce = true
	initSchema = ""
	initSource = "."

	err := runInit(initCmd, nil)
	require.NoError(t, err)
}

func TestInit_ExplicitSchema(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chdir(dir))

	buf := new(bytes.Buffer)
	initCmd.SetOut(buf)

	initForce = false
	initSchema = "python"
	initSource = "."

	err := runInit(initCmd, nil)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(dir, ConfigFileName))
	require.NoError(t, err)

	var cfg ProjectConfig
	require.NoError(t, json.Unmarshal(data, &cfg))
	assert.Equal(t, "python", cfg.Sources[0].Schema)
}

func TestInit_CustomSource(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chdir(dir))

	buf := new(bytes.Buffer)
	initCmd.SetOut(buf)

	initForce = false
	initSchema = ""
	initSource = "./data/mydb.db"

	err := runInit(initCmd, nil)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(dir, ConfigFileName))
	require.NoError(t, err)

	var cfg ProjectConfig
	require.NoError(t, json.Unmarshal(data, &cfg))
	assert.Equal(t, "./data/mydb.db", cfg.Sources[0].Path)
}
