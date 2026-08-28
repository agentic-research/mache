package cmd

// Preset-schema coverage stays with the presets themselves (schemas.go /
// infer.go, still cmd-resident until stage 6 of mache-96c378). The rest of
// the old config_test.go moved to internal/projcfg with its subjects.

import (
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/lang"
	"github.com/agentic-research/mache/internal/projcfg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveSchema_AllPresets(t *testing.T) {
	for name := range presetSchemas {
		t.Run(name, func(t *testing.T) {
			topo, err := projcfg.ResolveSchema(name, ".")
			require.NoError(t, err)
			require.NotNil(t, topo)
			assert.Equal(t, "v1", topo.Version)
		})
	}
}

// knownBrokenPresets maps preset name → tracking bead for selectors that
// don't compile against their tree-sitter grammar. The test skips these
// so CI stays green while the schemas are being repaired. Each bead lists
// the specific selector(s) involved.
//
// Remove an entry when the corresponding bead is closed and the selector
// compiles cleanly.
var knownBrokenPresets = map[string]string{}

// TestPresetSchemas_SelectorsCompile loads every preset schema whose key
// matches a registered tree-sitter language and verifies that each
// selector in the schema tree compiles as a tree-sitter query against
// that language. Bead mache-a21b69 — `TestResolveSchema_AllPresets`
// only validates JSON parsing, so a broken selector silently routes
// files to `_project_files/` instead of failing loudly.
//
// Data-format presets (cli, mcp, mcp-registry) use JSONPath selectors
// rather than tree-sitter and are skipped. Presets in
// knownBrokenPresets are skipped pending the linked beads.
// TestPresetSchemas_SelectorsCompile checks preset S-expression selectors are
// structurally well-formed. In-process CGO tree-sitter grammar compilation was
// removed (ADR-0012 step 4), so this can no longer compile each selector
// against a live grammar — that end-to-end validation now lives in the
// leyline-gated preset projection tests (preset_fixture_test.go). This remaining
// check catches gross breakage (empty, non-S-expression, unbalanced parens).
func TestPresetSchemas_SelectorsCompile(t *testing.T) {
	dataPresets := map[string]bool{"cli": true, "mcp": true, "mcp-registry": true}

	for name := range presetSchemas {
		if dataPresets[name] {
			continue
		}
		if lang.ForName(name) == nil {
			continue
		}
		t.Run(name, func(t *testing.T) {
			if bead, broken := knownBrokenPresets[name]; broken {
				t.Skipf("known-broken selectors — see %s", bead)
			}
			topo, err := loadPresetSchema(name)
			require.NoError(t, err)
			require.NotNil(t, topo)

			walkPresetNodes(t, topo.Nodes, name, func(node api.Node, path string) {
				sel := node.Selector
				if sel == "" || sel == "$" {
					return
				}
				assert.Equal(t, byte('('), sel[0],
					"preset %q selector at %s must be a tree-sitter S-expression (start with '('): %s", name, path, sel)
				depth := 0
				for _, r := range sel {
					switch r {
					case '(':
						depth++
					case ')':
						depth--
					}
					if depth < 0 {
						break
					}
				}
				assert.Equal(t, 0, depth,
					"preset %q selector at %s has unbalanced parentheses: %s", name, path, sel)
			})
		})
	}
}

func walkPresetNodes(t *testing.T, nodes []api.Node, parentPath string, fn func(api.Node, string)) {
	t.Helper()
	for i := range nodes {
		path := parentPath + "/" + nodes[i].Name
		fn(nodes[i], path)
		walkPresetNodes(t, nodes[i].Children, path, fn)
	}
}

func TestPresetNames(t *testing.T) {
	names := PresetNames()
	assert.Contains(t, names, "go")
	assert.Contains(t, names, "python")
	assert.Contains(t, names, "sql")
	assert.Len(t, names, len(presetSchemas))
	// Must be sorted (doc contract)
	assert.IsNonDecreasing(t, names)
}
