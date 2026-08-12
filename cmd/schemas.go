package cmd

import (
	"github.com/agentic-research/mache/api"
	publicschema "github.com/agentic-research/mache/schema"
)

// presetSchemas is a compatibility membership index for cmd call sites and
// tests. The public schema package owns the registry and embedded bytes.
var presetSchemas map[string]string

func init() {
	presetSchemas = make(map[string]string)
	for _, name := range publicschema.AvailablePresets() {
		presetSchemas[name] = name
	}
}

// PresetNames returns the sorted list of available preset schema names.
func PresetNames() []string {
	return publicschema.AvailablePresets()
}

// loadPresetSchema loads a bundled schema by preset name.
func loadPresetSchema(name string) (*api.Topology, error) {
	resolved, err := publicschema.LoadPreset(name)
	if err != nil {
		return nil, err
	}
	return resolved.Topology, nil
}
