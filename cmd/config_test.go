package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadProjectConfig_Valid(t *testing.T) {
	dir := t.TempDir()
	cfg := `{"sources": [{"path": ".", "schema": "go"}]}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(cfg), 0o644))

	got, err := loadProjectConfig(dir)
	require.NoError(t, err)
	require.Len(t, got.Sources, 1)
	assert.Equal(t, ".", got.Sources[0].Path)
	assert.Equal(t, "go", got.Sources[0].Schema)
}

func TestLoadProjectConfig_MultipleSources(t *testing.T) {
	dir := t.TempDir()
	cfg := `{"sources": [{"path": ".", "schema": "go"}, {"path": "./data.db"}]}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(cfg), 0o644))

	got, err := loadProjectConfig(dir)
	require.NoError(t, err)
	require.Len(t, got.Sources, 2)
	assert.Equal(t, "", got.Sources[1].Schema)
}

func TestLoadProjectConfig_NotFound(t *testing.T) {
	_, err := loadProjectConfig(t.TempDir())
	assert.True(t, os.IsNotExist(err))
}

func TestLoadProjectConfig_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ConfigFileName), []byte("{bad"), 0o644))

	_, err := loadProjectConfig(dir)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "parse")
}

func TestLoadProjectConfig_EmptySources(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(`{"sources": []}`), 0o644))

	_, err := loadProjectConfig(dir)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

func TestResolveSchema_Preset(t *testing.T) {
	topo, err := resolveSchema("go", ".")
	require.NoError(t, err)
	require.NotNil(t, topo)
	assert.Equal(t, "v1", topo.Version)
	assert.NotEmpty(t, topo.Nodes)
}

func TestResolveSchema_AllPresets(t *testing.T) {
	for name := range presetSchemas {
		t.Run(name, func(t *testing.T) {
			topo, err := resolveSchema(name, ".")
			require.NoError(t, err)
			require.NotNil(t, topo)
			assert.Equal(t, "v1", topo.Version)
		})
	}
}

func TestResolveSchema_RelativePath(t *testing.T) {
	dir := t.TempDir()
	schema := `{"version": "v1", "nodes": []}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "custom.json"), []byte(schema), 0o644))

	topo, err := resolveSchema("./custom.json", dir)
	require.NoError(t, err)
	require.NotNil(t, topo)
	assert.Equal(t, "v1", topo.Version)
}

func TestResolveSchema_AbsolutePath(t *testing.T) {
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "abs-schema.json")
	require.NoError(t, os.WriteFile(schemaPath, []byte(`{"version": "v1", "nodes": []}`), 0o644))

	topo, err := resolveSchema(schemaPath, "/other/dir")
	require.NoError(t, err)
	require.NotNil(t, topo)
}

func TestResolveSchema_Empty(t *testing.T) {
	topo, err := resolveSchema("", ".")
	require.NoError(t, err)
	assert.Nil(t, topo)
}

func TestResolveSchema_UnknownPreset(t *testing.T) {
	_, err := resolveSchema("fortran", ".")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "fortran")
}

func TestResolveDataSource_Relative(t *testing.T) {
	got := resolveDataSource(".", "/home/user/project")
	assert.Equal(t, "/home/user/project", got)
}

func TestResolveDataSource_RelativeSubdir(t *testing.T) {
	got := resolveDataSource("./data", "/home/user/project")
	assert.Equal(t, "/home/user/project/data", got)
}

func TestResolveDataSource_Absolute(t *testing.T) {
	got := resolveDataSource("/opt/data.db", "/home/user/project")
	assert.Equal(t, "/opt/data.db", got)
}

func TestDetectProjectType_GoProject(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"main.go", "util.go", "README.md"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(""), 0o644))
	}
	assert.Equal(t, "go", detectProjectType(dir))
}

func TestDetectProjectType_PythonProject(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"app.py", "utils.py", "setup.cfg"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(""), 0o644))
	}
	assert.Equal(t, "python", detectProjectType(dir))
}

func TestDetectProjectType_DBProject(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "data.db"), []byte(""), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte(""), 0o644))
	// .db takes priority — returns empty (no preset)
	assert.Equal(t, "", detectProjectType(dir))
}

func TestDetectProjectType_NoMatch(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte(""), 0o644))
	assert.Equal(t, "", detectProjectType(dir))
}

func TestWriteClaudeMCPConfig_Fresh(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, writeClaudeMCPConfig(dir, "mache"))

	data, err := os.ReadFile(filepath.Join(dir, ".claude", "mcp.json"))
	require.NoError(t, err)

	var cfg claudeMCPConfig
	require.NoError(t, json.Unmarshal(data, &cfg))
	require.Contains(t, cfg.MCPServers, "mache")

	var entry mcpServerEntry
	require.NoError(t, json.Unmarshal(cfg.MCPServers["mache"], &entry))
	assert.Equal(t, "mache", entry.Command)
	assert.Equal(t, []string{"serve"}, entry.Args)
}

func TestWriteClaudeMCPConfig_MergeExisting(t *testing.T) {
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, ".claude")
	require.NoError(t, os.MkdirAll(claudeDir, 0o755))

	existing := `{"mcpServers": {"github": {"command": "gh", "args": ["mcp"]}}}`
	require.NoError(t, os.WriteFile(filepath.Join(claudeDir, "mcp.json"), []byte(existing), 0o644))

	require.NoError(t, writeClaudeMCPConfig(dir, "/usr/local/bin/mache"))

	data, err := os.ReadFile(filepath.Join(claudeDir, "mcp.json"))
	require.NoError(t, err)

	var cfg claudeMCPConfig
	require.NoError(t, json.Unmarshal(data, &cfg))

	// Both entries should exist
	assert.Contains(t, cfg.MCPServers, "github")
	assert.Contains(t, cfg.MCPServers, "mache")

	var entry mcpServerEntry
	require.NoError(t, json.Unmarshal(cfg.MCPServers["mache"], &entry))
	assert.Equal(t, "/usr/local/bin/mache", entry.Command)
}

func TestPresetNames(t *testing.T) {
	names := PresetNames()
	assert.Contains(t, names, "go")
	assert.Contains(t, names, "python")
	assert.Contains(t, names, "sql")
	assert.Len(t, names, len(presetSchemas))
}
