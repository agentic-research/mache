package cmd

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/agentic-research/mache/api"
)

//go:embed schemas/*.json
var embeddedSchemas embed.FS

var presetSchemas = map[string]string{
	// Source-code languages (mapped from DetectLanguageFromExt)
	"go":         "schemas/go.json",
	"python":     "schemas/python.json",
	"rust":       "schemas/rust.json",
	"terraform":  "schemas/terraform.json",
	"sql":        "schemas/sql.json",
	"toml":       "schemas/toml.json",
	"yaml":       "schemas/yaml.json",
	"javascript": "schemas/javascript.json",
	"typescript": "schemas/typescript.json",
	"java":       "schemas/java.json",
	"c":          "schemas/c.json",
	"cpp":        "schemas/cpp.json",
	"ruby":       "schemas/ruby.json",
	"php":        "schemas/php.json",
	"kotlin":     "schemas/kotlin.json",
	"swift":      "schemas/swift.json",
	"scala":      "schemas/scala.json",
	"elixir":     "schemas/elixir.json",
	// Data-format presets (not auto-detected from file extensions)
	"cli":          "schemas/cli.json",
	"mcp":          "schemas/mcp.json",
	"mcp-registry": "schemas/mcp-registry.json",
}

// PresetNames returns the sorted list of available preset schema names.
func PresetNames() []string {
	names := make([]string, 0, len(presetSchemas))
	for k := range presetSchemas {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// loadPresetSchema loads a bundled schema by preset name.
func loadPresetSchema(name string) (*api.Topology, error) {
	path, ok := presetSchemas[name]
	if !ok {
		return nil, fmt.Errorf("unknown preset schema %q (available: %v)", name, PresetNames())
	}
	data, err := embeddedSchemas.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read embedded schema %q: %w", name, err)
	}
	var topo api.Topology
	if err := json.Unmarshal(data, &topo); err != nil {
		return nil, fmt.Errorf("parse embedded schema %q: %w", name, err)
	}
	return &topo, nil
}
