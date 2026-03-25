package cmd

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/lang"
)

//go:embed schemas/*.json
var embeddedSchemas embed.FS

// presetSchemas maps preset name → embedded schema path.
// Derived from the lang registry at init time — adding a language
// to internal/lang automatically adds its preset here.
var presetSchemas map[string]string

func init() {
	presetSchemas = make(map[string]string)
	for i := range lang.Registry {
		l := &lang.Registry[i]
		if l.PresetSchema != "" {
			presetSchemas[l.Name] = "schemas/" + l.PresetSchema + ".json"
		}
	}
	// Data-format presets (not auto-detected from file extensions)
	presetSchemas["cli"] = "schemas/cli.json"
	presetSchemas["mcp"] = "schemas/mcp.json"
	presetSchemas["mcp-registry"] = "schemas/mcp-registry.json"
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
