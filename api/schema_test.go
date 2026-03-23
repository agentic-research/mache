package api

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiagramDef_Unmarshal(t *testing.T) {
	raw := `{
		"version": "v1",
		"diagrams": {
			"architecture": { "layout": "TD" },
			"data-flow":    { "layout": "LR" }
		},
		"nodes": []
	}`

	var topo Topology
	err := json.Unmarshal([]byte(raw), &topo)
	require.NoError(t, err)

	require.Len(t, topo.Diagrams, 2)
	assert.Equal(t, "TD", topo.Diagrams["architecture"].Layout)
	assert.Equal(t, "LR", topo.Diagrams["data-flow"].Layout)
}

func TestDiagramDef_OmittedIsNil(t *testing.T) {
	raw := `{"version": "v1", "nodes": []}`

	var topo Topology
	err := json.Unmarshal([]byte(raw), &topo)
	require.NoError(t, err)
	assert.Nil(t, topo.Diagrams)
}

func TestDiagramDef_EmptyMap(t *testing.T) {
	raw := `{"version": "v1", "diagrams": {}, "nodes": []}`

	var topo Topology
	err := json.Unmarshal([]byte(raw), &topo)
	require.NoError(t, err)
	assert.NotNil(t, topo.Diagrams)
	assert.Empty(t, topo.Diagrams)
}

func TestDiagramDef_Marshal(t *testing.T) {
	topo := Topology{
		Version: "v1",
		Diagrams: map[string]DiagramDef{
			"system": {Layout: "BT"},
		},
	}

	data, err := json.Marshal(topo)
	require.NoError(t, err)

	var roundTrip Topology
	err = json.Unmarshal(data, &roundTrip)
	require.NoError(t, err)
	assert.Equal(t, "BT", roundTrip.Diagrams["system"].Layout)
}

func TestDiagramDef_CoexistsWithFileSets(t *testing.T) {
	raw := `{
		"version": "v1",
		"diagrams": {
			"deps": { "layout": "RL" }
		},
		"file_sets": {
			"common": [{"name": "source", "content_template": "{{.source}}"}]
		},
		"nodes": [
			{
				"name": "root",
				"selector": "$",
				"include": ["common"]
			}
		]
	}`

	var topo Topology
	err := json.Unmarshal([]byte(raw), &topo)
	require.NoError(t, err)

	assert.Equal(t, "RL", topo.Diagrams["deps"].Layout)
	require.Len(t, topo.FileSets["common"], 1)
	assert.Equal(t, "source", topo.FileSets["common"][0].Name)

	// ResolveIncludes should still work
	topo.ResolveIncludes()
	require.Len(t, topo.Nodes[0].Files, 1)
	assert.Equal(t, "source", topo.Nodes[0].Files[0].Name)
}

func TestDiagramDef_AllLayouts(t *testing.T) {
	for _, layout := range []string{"TD", "LR", "BT", "RL"} {
		t.Run(layout, func(t *testing.T) {
			def := DiagramDef{Layout: layout}
			data, err := json.Marshal(def)
			require.NoError(t, err)

			var rt DiagramDef
			err = json.Unmarshal(data, &rt)
			require.NoError(t, err)
			assert.Equal(t, layout, rt.Layout)
		})
	}
}
