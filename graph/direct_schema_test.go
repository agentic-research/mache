package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/api"
)

// The direct path honours a schema whose every node below a root means
// "this record", and refuses one that selects inside a record or names a
// root by template, saying which node and where to go instead.
func TestCheckDirectSchema(t *testing.T) {
	record := func(sel string) api.Node {
		return api.Node{Name: "{{.name}}", Selector: sel, Files: []api.Leaf{{Name: "f", ContentTemplate: "x"}}}
	}
	for _, sel := range []string{"", "$", "$[*]"} {
		schema := &api.Topology{Nodes: []api.Node{{Name: "root", Selector: "$.ignored[*]", Children: []api.Node{record(sel)}}}}
		assert.NoError(t, checkDirectSchema(schema), "selector %q means the record; a root's selector is ignored", sel)
	}

	for name, c := range map[string]struct {
		schema *api.Topology
		want   string
	}{
		"selector inside a record": {
			&api.Topology{Nodes: []api.Node{{Name: "servers", Children: []api.Node{{
				Name: "{{.name}}", Selector: "$[*]", Children: []api.Node{record("$.tools[*]")},
			}}}}},
			`node "servers/{{.name}}/{{.name}}" selects "$.tools[*]"`,
		},
		"a filter": {
			&api.Topology{Nodes: []api.Node{{Name: "beads", Children: []api.Node{record(`$[?(@.kind == 'bead')]`)}}}},
			`selects "$[?(@.kind == 'bead')]"`,
		},
		"templated root": {
			&api.Topology{Nodes: []api.Node{{Name: "{{.area}}", Files: []api.Leaf{{Name: "f"}}}}},
			`root "{{.area}}" has a templated name`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := checkDirectSchema(c.schema)
			require.ErrorIs(t, err, ErrDirectSchemaUnsupported)
			assert.Contains(t, err.Error(), c.want)
			assert.Contains(t, err.Error(), "--out", "says where to go instead")
		})
	}
}
