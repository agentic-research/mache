package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/agentic-research/mache/api"
)

// ConfigFileName is the convention file looked up by `mache serve`.
const ConfigFileName = ".mache.json"

// ProjectConfig represents the .mache.json project configuration.
type ProjectConfig struct {
	Sources []SourceConfig `json:"sources"`
}

// SourceConfig describes a single data source within a project.
type SourceConfig struct {
	Path   string `json:"path"`
	Schema string `json:"schema,omitempty"`
}

// loadProjectConfig reads .mache.json from the given directory.
func loadProjectConfig(dir string) (*ProjectConfig, error) {
	path := filepath.Join(dir, ConfigFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg ProjectConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(cfg.Sources) == 0 {
		return nil, fmt.Errorf("%s: sources array is empty", path)
	}
	return &cfg, nil
}

// resolveSchema resolves a schema reference to a Topology.
// It handles three forms:
//   - Preset name: "go", "python", etc.
//   - Relative path: "./custom-schema.json" (resolved against configDir)
//   - Absolute path: "/path/to/schema.json"
//
// Returns nil if schemaRef is empty (caller should use inference or default).
func resolveSchema(schemaRef, configDir string) (*api.Topology, error) {
	if schemaRef == "" {
		return nil, nil
	}

	// Check preset first
	if _, ok := presetSchemas[schemaRef]; ok {
		return loadPresetSchema(schemaRef)
	}

	// Treat as file path
	schemaPath := schemaRef
	if !filepath.IsAbs(schemaPath) {
		schemaPath = filepath.Join(configDir, schemaPath)
	}

	data, err := os.ReadFile(schemaPath)
	if err != nil {
		return nil, fmt.Errorf("read schema %q: %w", schemaPath, err)
	}
	var topo api.Topology
	if err := json.Unmarshal(data, &topo); err != nil {
		return nil, fmt.Errorf("parse schema %q: %w", schemaPath, err)
	}
	return &topo, nil
}

// resolveDataSource resolves a data source path relative to configDir.
func resolveDataSource(sourcePath, configDir string) string {
	if filepath.IsAbs(sourcePath) {
		return sourcePath
	}
	return filepath.Join(configDir, sourcePath)
}

// claudeMCPConfig represents the .claude/mcp.json structure.
type claudeMCPConfig struct {
	MCPServers map[string]json.RawMessage `json:"mcpServers"`
}

type mcpServerEntry struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// writeClaudeMCPConfig writes or merges a mache entry into .claude/mcp.json.
func writeClaudeMCPConfig(projectDir, macheCommand string) error {
	claudeDir := filepath.Join(projectDir, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		return fmt.Errorf("create .claude dir: %w", err)
	}

	mcpPath := filepath.Join(claudeDir, "mcp.json")

	var cfg claudeMCPConfig
	if data, err := os.ReadFile(mcpPath); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return fmt.Errorf("parse existing %s: %w", mcpPath, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", mcpPath, err)
	}

	if cfg.MCPServers == nil {
		cfg.MCPServers = make(map[string]json.RawMessage)
	}

	entry := mcpServerEntry{
		Command: macheCommand,
		Args:    []string{"serve"},
	}
	entryJSON, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal mache entry: %w", err)
	}
	cfg.MCPServers["mache"] = entryJSON

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal mcp config: %w", err)
	}
	if err := os.WriteFile(mcpPath, append(out, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", mcpPath, err)
	}
	return nil
}

// detectProjectType scans a directory and returns the best-fit schema preset name.
// Returns empty string if no preset matches (caller should omit schema).
func detectProjectType(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}

	counts := map[string]int{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		switch ext {
		case ".go":
			counts["go"]++
		case ".py":
			counts["python"]++
		case ".sql":
			counts["sql"]++
		case ".db":
			counts["db"]++
		}
	}

	// .db files mean custom data — no preset
	if counts["db"] > 0 {
		return ""
	}

	best := ""
	bestCount := 0
	for lang, count := range counts {
		if count > bestCount {
			best = lang
			bestCount = count
		}
	}
	return best
}
