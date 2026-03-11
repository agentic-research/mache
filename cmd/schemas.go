package cmd

import (
	"embed"
	"encoding/json"
	"fmt"

	"github.com/agentic-research/mache/api"
)

//go:embed schemas/*.json
var embeddedSchemas embed.FS

var presetSchemas = map[string]string{
	"go":           "schemas/go.json",
	"python":       "schemas/python.json",
	"sql":          "schemas/sql.json",
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
