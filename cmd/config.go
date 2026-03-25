package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/lang"
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
		// Containment check: relative paths must stay within configDir
		if err := checkPathContainment(schemaPath, configDir); err != nil {
			return nil, err
		}
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
// Absolute paths are returned as-is. Relative paths are checked for
// containment within configDir to prevent path traversal.
func resolveDataSource(sourcePath, configDir string) (string, error) {
	if filepath.IsAbs(sourcePath) {
		return sourcePath, nil
	}
	resolved := filepath.Join(configDir, sourcePath)
	if err := checkPathContainment(resolved, configDir); err != nil {
		return "", err
	}
	return resolved, nil
}

// checkPathContainment verifies that resolved is within or equal to base.
// Prevents path traversal attacks from untrusted .mache.json files.
func checkPathContainment(resolved, base string) error {
	absResolved, err := filepath.Abs(resolved)
	if err != nil {
		return fmt.Errorf("resolve absolute path %q: %w", resolved, err)
	}
	absBase, err := filepath.Abs(base)
	if err != nil {
		return fmt.Errorf("resolve absolute path %q: %w", base, err)
	}
	// Check that resolved path starts with base path
	rel, err := filepath.Rel(absBase, absResolved)
	if err != nil {
		return fmt.Errorf("path %q escapes project directory %q", resolved, base)
	}
	if strings.HasPrefix(rel, "..") {
		return fmt.Errorf("path %q escapes project directory %q", resolved, base)
	}
	return nil
}

// mcpServerEntry is the mache entry written into MCP config files.
type mcpServerEntry struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// writeClaudeMCPConfig writes or merges a mache entry into .claude/mcp.json.
// Uses map[string]any as root type to preserve unknown top-level keys.
func writeClaudeMCPConfig(projectDir, macheCommand string) error {
	claudeDir := filepath.Join(projectDir, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		return fmt.Errorf("create .claude dir: %w", err)
	}

	mcpPath := filepath.Join(claudeDir, "mcp.json")

	root := make(map[string]any)
	if data, err := os.ReadFile(mcpPath); err == nil {
		if err := json.Unmarshal(data, &root); err != nil {
			return fmt.Errorf("parse existing %s: %w", mcpPath, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", mcpPath, err)
	}

	servers, _ := root["mcpServers"].(map[string]any)
	if servers == nil {
		servers = make(map[string]any)
	}

	servers["mache"] = mcpServerEntry{
		Command: macheCommand,
		Args:    []string{"serve"},
	}
	root["mcpServers"] = servers

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal mcp config: %w", err)
	}
	if err := os.WriteFile(mcpPath, append(out, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", mcpPath, err)
	}
	return nil
}

// writeClaudeMD writes a .claude/CLAUDE.md that describes the mache MCP tools
// so Claude Code automatically knows how to use them.
func writeClaudeMD(projectDir, schemaPreset string) error {
	claudeDir := filepath.Join(projectDir, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		return fmt.Errorf("create .claude dir: %w", err)
	}

	mdPath := filepath.Join(claudeDir, "CLAUDE.md")

	// If file exists, check if it already has mache section
	if existing, err := os.ReadFile(mdPath); err == nil {
		if strings.Contains(string(existing), "## Mache") {
			// Already has mache section, don't duplicate
			return nil
		}
		// Append to existing file
		content := string(existing) + "\n" + generateMacheCLAUDESection(schemaPreset)
		return os.WriteFile(mdPath, []byte(content), 0o644)
	}

	// Create new file
	return os.WriteFile(mdPath, []byte(generateMacheCLAUDESection(schemaPreset)), 0o644)
}

func generateMacheCLAUDESection(schemaPreset string) string {
	var sb strings.Builder
	sb.WriteString("## Mache — Structured Code Index\n\n")
	sb.WriteString("This project has a mache MCP server configured. ")
	sb.WriteString("Use the mache tools to explore the codebase structure without reading raw files.\n\n")
	sb.WriteString("### Available Tools\n\n")
	sb.WriteString("| Tool | Purpose |\n")
	sb.WriteString("|------|--------|\n")
	sb.WriteString("| `list_directory` | Browse the projected directory tree (empty path = root) |\n")
	sb.WriteString("| `read_file` | Read content of a projected file node |\n")
	sb.WriteString("| `find_callers` | Find all nodes referencing a symbol/token |\n")
	sb.WriteString("| `find_callees` | Find all symbols called by a construct |\n")
	sb.WriteString("| `search` | Search symbols by pattern (SQL LIKE: `%auth%`) |\n")
	sb.WriteString("| `get_communities` | Detect clusters of co-referencing nodes |\n")
	sb.WriteString("\n### Workflow\n\n")
	sb.WriteString("1. Start with `list_directory` (empty path) to see top-level structure\n")
	sb.WriteString("2. Drill into directories of interest with `list_directory`\n")
	sb.WriteString("3. Read specific files with `read_file`\n")
	sb.WriteString("4. Use `search` to find symbols across the codebase\n")
	sb.WriteString("5. Use `find_callers`/`find_callees` to trace dependencies\n")
	if schemaPreset != "" {
		fmt.Fprintf(&sb, "\nSchema preset: **%s**\n", schemaPreset)
	}
	return sb.String()
}

// editorConfig describes how to register an MCP server with a specific editor.
type editorConfig struct {
	Name         string // e.g. "Cursor"
	ConfigPath   string // absolute path to config file
	ServerKey    string // JSON key holding server map ("mcpServers", "servers", "context_servers")
	EntryFunc    func(binaryPath string) map[string]any
	SharedConfig bool // true if config file is shared with other settings (may contain comments)
}

// detectEditors returns configs for all editors found on the system.
func detectEditors(binaryPath string) []editorConfig {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}

	editors := []editorConfig{
		{
			Name:       "Cursor",
			ConfigPath: filepath.Join(home, ".cursor", "mcp.json"),
			ServerKey:  "mcpServers",
			EntryFunc:  mcpServersEntry,
		},
		{
			Name:       "Windsurf",
			ConfigPath: filepath.Join(home, ".codeium", "windsurf", "mcp_config.json"),
			ServerKey:  "mcpServers",
			EntryFunc:  mcpServersEntry,
		},
		{
			Name:         "Gemini CLI",
			ConfigPath:   filepath.Join(home, ".gemini", "settings.json"),
			ServerKey:    "mcpServers",
			EntryFunc:    mcpServersEntry,
			SharedConfig: true,
		},
		{
			Name:         "Zed",
			ConfigPath:   filepath.Join(home, ".config", "zed", "settings.json"),
			ServerKey:    "context_servers",
			SharedConfig: true,
			EntryFunc: func(bp string) map[string]any {
				return map[string]any{"source": "custom", "command": bp, "args": []string{"serve"}}
			},
		},
	}

	// VS Code path is OS-dependent
	vscodePath := ""
	switch runtime.GOOS {
	case "darwin":
		vscodePath = filepath.Join(home, "Library", "Application Support", "Code", "User", "mcp.json")
	case "linux":
		vscodePath = filepath.Join(home, ".config", "Code", "User", "mcp.json")
	}
	if vscodePath != "" {
		editors = append(editors, editorConfig{
			Name:       "VS Code",
			ConfigPath: vscodePath,
			ServerKey:  "servers",
			EntryFunc: func(bp string) map[string]any {
				return map[string]any{"type": "stdio", "command": bp, "args": []string{"serve"}}
			},
		})
	}

	return editors
}

func mcpServersEntry(binaryPath string) map[string]any {
	return map[string]any{"command": binaryPath, "args": []string{"serve"}}
}

// registerEditorMCP upserts mache into an editor's MCP config file.
// Only writes if the config file's parent directory already exists
// (i.e. the editor is installed). Returns true if registration was performed.
// For shared config files (may contain JSONC comments), invalid JSON causes
// a skip rather than overwrite, and a warning is returned via the second value.
func registerEditorMCP(ec editorConfig, binaryPath string) (ok bool, warning string) {
	// Only register if the editor's config directory exists
	configDir := filepath.Dir(ec.ConfigPath)
	if _, err := os.Stat(configDir); os.IsNotExist(err) {
		return false, ""
	}

	root := make(map[string]any)
	if data, err := os.ReadFile(ec.ConfigPath); err == nil {
		if jsonErr := json.Unmarshal(data, &root); jsonErr != nil {
			// Invalid JSON — start fresh for dedicated config files,
			// skip for shared settings files (may contain // comments)
			if ec.SharedConfig {
				return false, fmt.Sprintf("%s has non-standard JSON (comments?) — add mache manually", ec.ConfigPath)
			}
			root = make(map[string]any)
		}
	}

	servers, _ := root[ec.ServerKey].(map[string]any)
	if servers == nil {
		servers = make(map[string]any)
	}

	servers["mache"] = ec.EntryFunc(binaryPath)
	root[ec.ServerKey] = servers

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return false, ""
	}
	if err := os.WriteFile(ec.ConfigPath, append(out, '\n'), 0o644); err != nil {
		return false, ""
	}
	return true, ""
}

// claudeCLIRegister is the function that performs Claude Code CLI registration.
// Replaced in tests to avoid real side effects.
var claudeCLIRegister = registerClaudeCodeCLIImpl

// registerClaudeCodeCLI registers mache via `claude mcp add` if the CLI is available.
func registerClaudeCodeCLI(binaryPath string) bool {
	return claudeCLIRegister(binaryPath)
}

func registerClaudeCodeCLIImpl(binaryPath string) bool {
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return false
	}
	// Remove first (may fail if not registered — that's fine)
	_ = exec.Command(claudePath, "mcp", "remove", "-s", "user", "mache").Run()
	err = exec.Command(claudePath, "mcp", "add", "--scope", "user", "mache", "--", binaryPath, "serve").Run()
	return err == nil
}

// registerAllEditors registers mache with all detected editors.
// Returns the names of editors that were successfully registered.
func registerAllEditors(w io.Writer, binaryPath string) {
	// Claude Code via CLI
	if registerClaudeCodeCLI(binaryPath) {
		_, _ = fmt.Fprintln(w, "  [Claude Code] registered via CLI (scope: user)")
	}

	// File-based editors
	for _, ec := range detectEditors(binaryPath) {
		ok, warn := registerEditorMCP(ec, binaryPath)
		if ok {
			_, _ = fmt.Fprintf(w, "  [%s] updated %s\n", ec.Name, ec.ConfigPath)
		} else if warn != "" {
			_, _ = fmt.Fprintf(w, "  [%s] skipped: %s\n", ec.Name, warn)
		}
	}
}

// sentinelFiles maps sentinel filenames to their language preset.
// Derived from the lang registry at init time — adding SentinelFiles
// to a language in internal/lang automatically updates detection here.
var sentinelFiles map[string]string

func init() {
	sentinelFiles = make(map[string]string)
	for i := range lang.Registry {
		l := &lang.Registry[i]
		for _, sf := range l.SentinelFiles {
			sentinelFiles[sf] = l.Name
		}
	}
}

// detectProjectType scans a directory and returns the best-fit schema preset name.
// Checks sentinel files first (go.mod, pyproject.toml, etc.), then falls back
// to counting file extensions in the top-level directory.
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
		name := e.Name()

		// Sentinel files take priority
		if preset, ok := sentinelFiles[name]; ok {
			return preset
		}

		ext := strings.ToLower(filepath.Ext(name))
		if ext == ".db" {
			counts["db"]++
			continue
		}
		if l := lang.ForExt(ext); l != nil && l.PresetSchema != "" {
			counts[l.Name]++
		}
	}

	// .db files mean custom data — no preset
	if counts["db"] > 0 {
		return ""
	}

	best := ""
	bestCount := 0
	for l, count := range counts {
		if count > bestCount || (count == bestCount && l < best) {
			best = l
			bestCount = count
		}
	}
	return best
}
