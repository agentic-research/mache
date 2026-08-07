package graph

import (
	"strings"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWalkSchemaLevels_MultipleStaticChildren — bead mache-1422c5.
//
// FALSIFIABLE: prior implementation always descended via children[0].
// Build a go-schema-like topology where one level has six static children
// (imports, functions, methods, types, constants, variables), each with
// distinct file names. Walk paths under each. With the old code most paths
// resolve to the wrong level; the file lookup either misses or hits the
// wrong leaf.
//
// With pickChildLevel, each segment routes to the matching static child.
func TestWalkSchemaLevels_MultipleStaticChildren(t *testing.T) {
	// Schema:
	//   pkg/                            (static)
	//     {{.pkgname}}/                 (dynamic)
	//       imports/    -> file: list
	//       functions/  -> file: source, doc
	//       methods/    -> file: source, doc
	//       types/      -> file: source, spec
	//       constants/  -> file: value
	//       variables/  -> file: value
	topology := &api.Topology{
		Version: api.SchemaVersion,
		Nodes: []api.Node{{
			Name: "pkg",
			Children: []api.Node{{
				Name: "{{.pkgname}}",
				Children: []api.Node{
					{Name: "imports", Files: []api.Leaf{{Name: "list"}}},
					{Name: "functions", Children: []api.Node{{
						Name:  "{{.fname}}",
						Files: []api.Leaf{{Name: "source"}, {Name: "doc"}},
					}}},
					{Name: "methods", Children: []api.Node{{
						Name:  "{{.mname}}",
						Files: []api.Leaf{{Name: "source"}, {Name: "doc"}},
					}}},
					{Name: "types", Children: []api.Node{{
						Name:  "{{.tname}}",
						Files: []api.Leaf{{Name: "source"}, {Name: "spec"}},
					}}},
					{Name: "constants", Children: []api.Node{{
						Name:  "{{.cname}}",
						Files: []api.Leaf{{Name: "value"}},
					}}},
					{Name: "variables", Children: []api.Node{{
						Name:  "{{.vname}}",
						Files: []api.Leaf{{Name: "value"}},
					}}},
				},
			}},
		}},
	}

	levels := compileLevels(topology)
	require.NotEmpty(t, levels)

	cases := []struct {
		path         string
		wantFile     string // empty if path is a directory
		wantLeafKind string
	}{
		{"pkg/foo/types/Config/spec", "spec", "types"},
		{"pkg/foo/types/Config/source", "source", "types"},
		{"pkg/foo/functions/Validate/source", "source", "functions"},
		{"pkg/foo/functions/Validate/doc", "doc", "functions"},
		{"pkg/foo/methods/Receiver.Method/doc", "doc", "methods"},
		{"pkg/foo/imports/list", "list", "imports"},
		{"pkg/foo/constants/MaxSize/value", "value", "constants"},
		{"pkg/foo/variables/globalCfg/value", "value", "variables"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			segments := strings.Split(tc.path, "/")
			level, leaf := walkSchemaLevels(levels, segments)
			require.NotNil(t, level, "level must resolve for %q", tc.path)
			require.NotNil(t, leaf, "leaf must resolve for %q", tc.path)
			assert.Equal(t, tc.wantFile, leaf.Name)
		})
	}
}

// TestWalkSchemaLevels_DynamicFallback verifies pickChildLevel falls back
// to the dynamic child when no static child matches the segment.
func TestWalkSchemaLevels_DynamicFallback(t *testing.T) {
	topology := &api.Topology{
		Version: api.SchemaVersion,
		Nodes: []api.Node{{
			Name: "data",
			Children: []api.Node{{
				Name:  "{{.id}}",
				Files: []api.Leaf{{Name: "record"}},
			}},
		}},
	}
	levels := compileLevels(topology)

	level, leaf := walkSchemaLevels(levels, []string{"data", "abc123", "record"})
	require.NotNil(t, level)
	require.NotNil(t, leaf)
	assert.Equal(t, "record", leaf.Name)
}

// TestPickChildLevel_PreferStaticName verifies the helper directly.
func TestPickChildLevel_PreferStaticName(t *testing.T) {
	// Two static + one dynamic
	a := &schemaLevel{isStatic: true, staticName: "alpha"}
	b := &schemaLevel{isStatic: true, staticName: "beta"}
	dyn := &schemaLevel{isStatic: false, nameRaw: "{{.x}}"}
	children := []*schemaLevel{a, b, dyn}

	assert.Same(t, a, pickChildLevel(children, "alpha"))
	assert.Same(t, b, pickChildLevel(children, "beta"))
	// Unknown segment falls through to the first dynamic child.
	assert.Same(t, dyn, pickChildLevel(children, "anything-else"))
	// Empty children → nil.
	assert.Nil(t, pickChildLevel(nil, "x"))
}
