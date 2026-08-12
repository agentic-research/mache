// Package schema loads Mache topology schemas from bundled presets or files.
package schema

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/lang"
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

// Parse decodes a topology schema.
func Parse(data []byte) (*api.Topology, error) {
	var topology api.Topology
	if err := json.Unmarshal(data, &topology); err != nil {
		return nil, fmt.Errorf("parse schema: %w", err)
	}
	return &topology, nil
}

// Preset loads a bundled schema by name.
func Preset(name string) (*Resolution, error) {
	path, ok := presetPaths[name]
	if !ok {
		return nil, fmt.Errorf("unknown preset schema %q (available: %v)", name, PresetNames())
	}
	data, err := presetFiles.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read embedded schema %q: %w", name, err)
	}
	topology, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parse embedded schema %q: %w", name, err)
	}
	resolution := &Resolution{Topology: topology}
	if language := lang.ForName(name); language != nil && language.Name == name {
		resolution.Languages = []string{language.Name}
	}
	return resolution, nil
}

// PresetNames returns the sorted names of all bundled schemas.
func PresetNames() []string {
	names := make([]string, 0, len(presetPaths))
	for name := range presetPaths {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// IsPreset reports whether ref names a bundled schema.
func IsPreset(ref string) bool {
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
	if IsPreset(ref) {
		return Preset(ref)
	}

	path := ref
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, path)
		if err := checkContainment(path, baseDir); err != nil {
			return nil, err
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read schema %q: %w", path, err)
	}
	topology, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parse schema %q: %w", path, err)
	}
	return &Resolution{Topology: topology}, nil
}

func checkContainment(path, base string) error {
	resolvedPath, err := evalOrAbs(path)
	if err != nil {
		return fmt.Errorf("resolve absolute path %q: %w", path, err)
	}
	resolvedBase, err := evalOrAbs(base)
	if err != nil {
		return fmt.Errorf("resolve absolute path %q: %w", base, err)
	}
	rel, err := filepath.Rel(resolvedBase, resolvedPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path %q escapes project directory %q", path, base)
	}
	return nil
}

func evalOrAbs(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}

	dir := abs
	var tail []string
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			return abs, nil
		}
		tail = append(tail, filepath.Base(dir))
		dir = parent
		if _, err := os.Stat(dir); err == nil {
			resolved, err := filepath.EvalSymlinks(dir)
			if err != nil {
				return "", err
			}
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved, nil
		}
	}
}
