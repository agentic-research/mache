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

func TestWriteClaudeMD_Fresh(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, writeClaudeMD(dir, "go"))

	data, err := os.ReadFile(filepath.Join(dir, ".claude", "CLAUDE.md"))
	require.NoError(t, err)
	content := string(data)
	assert.Contains(t, content, "## Mache")
	assert.Contains(t, content, "list_directory")
	assert.Contains(t, content, "find_callers")
	assert.Contains(t, content, "search")
	assert.Contains(t, content, "**go**")
}

func TestWriteClaudeMD_NoSchema(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, writeClaudeMD(dir, ""))

	data, err := os.ReadFile(filepath.Join(dir, ".claude", "CLAUDE.md"))
	require.NoError(t, err)
	assert.NotContains(t, string(data), "Schema preset:")
}

func TestPresetNames(t *testing.T) {
	names := PresetNames()
	assert.Contains(t, names, "go")
	assert.Contains(t, names, "python")
	assert.Contains(t, names, "sql")
	assert.Len(t, names, len(presetSchemas))
}

func TestRegisterEditorMCP_Fresh(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, ".editor")
	require.NoError(t, os.MkdirAll(configDir, 0o755))

	ec := editorConfig{
		Name:       "TestEditor",
		ConfigPath: filepath.Join(configDir, "mcp.json"),
		ServerKey:  "mcpServers",
		EntryFunc:  mcpServersEntry,
	}

	ok := registerEditorMCP(ec, "/usr/local/bin/mache")
	assert.True(t, ok)

	data, err := os.ReadFile(ec.ConfigPath)
	require.NoError(t, err)

	var root map[string]any
	require.NoError(t, json.Unmarshal(data, &root))
	servers := root["mcpServers"].(map[string]any)
	mache := servers["mache"].(map[string]any)
	assert.Equal(t, "/usr/local/bin/mache", mache["command"])
}

func TestRegisterEditorMCP_MergeExisting(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, ".editor")
	require.NoError(t, os.MkdirAll(configDir, 0o755))

	configPath := filepath.Join(configDir, "mcp.json")
	existing := `{"mcpServers": {"other-tool": {"command": "other"}}}`
	require.NoError(t, os.WriteFile(configPath, []byte(existing), 0o644))

	ec := editorConfig{
		Name:       "TestEditor",
		ConfigPath: configPath,
		ServerKey:  "mcpServers",
		EntryFunc:  mcpServersEntry,
	}

	ok := registerEditorMCP(ec, "/usr/local/bin/mache")
	assert.True(t, ok)

	data, err := os.ReadFile(configPath)
	require.NoError(t, err)

	var root map[string]any
	require.NoError(t, json.Unmarshal(data, &root))
	servers := root["mcpServers"].(map[string]any)
	assert.Contains(t, servers, "other-tool")
	assert.Contains(t, servers, "mache")
}

func TestRegisterEditorMCP_MissingDir(t *testing.T) {
	ec := editorConfig{
		Name:       "Missing",
		ConfigPath: filepath.Join(t.TempDir(), "nonexistent", "mcp.json"),
		ServerKey:  "mcpServers",
		EntryFunc:  mcpServersEntry,
	}
	assert.False(t, registerEditorMCP(ec, "mache"))
}

func TestRegisterEditorMCP_CustomServerKey(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, ".zed")
	require.NoError(t, os.MkdirAll(configDir, 0o755))

	ec := editorConfig{
		Name:       "Zed",
		ConfigPath: filepath.Join(configDir, "settings.json"),
		ServerKey:  "context_servers",
		EntryFunc: func(bp string) map[string]any {
			return map[string]any{"source": "custom", "command": bp, "args": []string{"serve"}}
		},
	}

	ok := registerEditorMCP(ec, "/usr/local/bin/mache")
	assert.True(t, ok)

	data, err := os.ReadFile(ec.ConfigPath)
	require.NoError(t, err)

	var root map[string]any
	require.NoError(t, json.Unmarshal(data, &root))
	servers := root["context_servers"].(map[string]any)
	mache := servers["mache"].(map[string]any)
	assert.Equal(t, "custom", mache["source"])
	assert.Equal(t, "/usr/local/bin/mache", mache["command"])
}

func TestRegisterEditorMCP_InvalidJSON_SharedSettings(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, ".zed")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	configPath := filepath.Join(configDir, "settings.json")
	require.NoError(t, os.WriteFile(configPath, []byte("{invalid"), 0o644))

	ec := editorConfig{
		Name:       "Zed",
		ConfigPath: configPath,
		ServerKey:  "context_servers",
		EntryFunc:  mcpServersEntry,
	}
	// Zed skips on invalid JSON (shared settings file)
	assert.False(t, registerEditorMCP(ec, "mache"))
}

func TestRegisterEditorMCP_InvalidJSON_DedicatedConfig(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, ".cursor")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	configPath := filepath.Join(configDir, "mcp.json")
	require.NoError(t, os.WriteFile(configPath, []byte("{invalid"), 0o644))

	ec := editorConfig{
		Name:       "Cursor",
		ConfigPath: configPath,
		ServerKey:  "mcpServers",
		EntryFunc:  mcpServersEntry,
	}
	// Cursor starts fresh on invalid JSON (dedicated config file)
	ok := registerEditorMCP(ec, "/usr/bin/mache")
	assert.True(t, ok)

	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	var root map[string]any
	require.NoError(t, json.Unmarshal(data, &root))
	assert.Contains(t, root["mcpServers"].(map[string]any), "mache")
}

func TestDetectEditors_ReturnsKnownEditors(t *testing.T) {
	editors := detectEditors("/usr/local/bin/mache")
	// Should return at least Cursor, Windsurf, Gemini CLI, Zed, and VS Code on darwin
	names := make(map[string]bool)
	for _, e := range editors {
		names[e.Name] = true
	}
	assert.True(t, names["Cursor"])
	assert.True(t, names["Windsurf"])
	assert.True(t, names["Zed"])
}

func TestRegisterAllEditors_Output(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// Create a fake Cursor dir
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".cursor"), 0o755))

	var buf bytes.Buffer
	registerAllEditors(&buf, "/usr/local/bin/mache")

	output := buf.String()
	assert.Contains(t, output, "[Cursor]")
	assert.Contains(t, output, "mcp.json")
}
