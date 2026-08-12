// Package schema loads Mache topology schemas from bundled presets or files.
package schema

import (
	"embed"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/lang"
	"github.com/agentic-research/mache/internal/pathguard"
)

//go:embed presets/*.json
var presetFiles embed.FS

// Resolution is a loaded topology plus language identity known from its
// reference. Languages is populated for language presets because the preset
// JSON intentionally carries no Node.Language filters.
type Resolution struct {
	Topology  *api.Topology
	Languages []string
}

var presetPaths = buildPresetPaths()

func buildPresetPaths() map[string]string {
	paths := make(map[string]string, len(lang.Registry)+3)
	for i := range lang.Registry {
		language := &lang.Registry[i]
		if language.PresetSchema != "" {
			paths[language.Name] = "presets/" + language.PresetSchema + ".json"
		}
	}
	paths["cli"] = "presets/cli.json"
	paths["mcp"] = "presets/mcp.json"
	paths["mcp-registry"] = "presets/mcp-registry.json"
	return paths
}

// ParseTopology decodes a topology schema.
func ParseTopology(data []byte) (*api.Topology, error) {
	var topology api.Topology
	if err := json.Unmarshal(data, &topology); err != nil {
		return nil, fmt.Errorf("parse schema: %w", err)
	}
	return &topology, nil
}

// LoadPreset loads a bundled schema by name.
func LoadPreset(name string) (*Resolution, error) {
	path, ok := presetPaths[name]
	if !ok {
		return nil, fmt.Errorf("unknown preset schema %q (available: %v)", name, AvailablePresets())
	}
	data, err := presetFiles.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read embedded schema %q: %w", name, err)
	}
	topology, err := ParseTopology(data)
	if err != nil {
		return nil, fmt.Errorf("parse embedded schema %q: %w", name, err)
	}
	resolution := &Resolution{Topology: topology}
	if language := lang.ForName(name); language != nil && language.Name == name {
		resolution.Languages = []string{language.Name}
	}
	return resolution, nil
}

// AvailablePresets returns the sorted names of all bundled schemas.
func AvailablePresets() []string {
	return slices.Sorted(maps.Keys(presetPaths))
}

func isPreset(ref string) bool {
	_, ok := presetPaths[ref]
	return ok
}

// Resolve loads a preset name, absolute schema path, or schema path relative
// to baseDir. Relative paths must remain inside baseDir after symlink
// resolution. An empty ref resolves to an empty Resolution.
func Resolve(ref, baseDir string) (*Resolution, error) {
	if ref == "" {
		return &Resolution{}, nil
	}
	if isPreset(ref) {
		return LoadPreset(ref)
	}

	path := ref
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, path)
		if err := pathguard.RequireContained(path, baseDir); err != nil {
			return nil, err
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read schema %q: %w", path, err)
	}
	topology, err := ParseTopology(data)
	if err != nil {
		return nil, fmt.Errorf("parse schema %q: %w", path, err)
	}
	return &Resolution{Topology: topology}, nil
}
