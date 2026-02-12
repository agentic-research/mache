package ingest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEngine_IngestJson(t *testing.T) {
	schema := &api.Topology{
		Nodes: []api.Node{
			{
				Name:     "users",
				Selector: "$", // Root object
				Children: []api.Node{
					{
						Name:     "{{.name}}",
						Selector: "users[*]", // Relative to root
						Files: []api.Leaf{
							{
								Name:            "role",
								ContentTemplate: "{{.role}}",
							},
						},
					},
				},
			},
		},
	}

	// Create temporary file
	tmpDir := t.TempDir()
	dataFile := filepath.Join(tmpDir, "data.json")
	err := os.WriteFile(dataFile, []byte(`
{
  "users": [
    {"name": "Alice", "role": "admin"},
    {"name": "Bob", "role": "user"}
  ]
}
`), 0o644)
	require.NoError(t, err)

	store := graph.NewMemoryStore()
	engine := NewEngine(schema, store)

	err = engine.Ingest(dataFile)
	require.NoError(t, err)

	// Verify Graph
	// Should have:
	// /users
	// /users/Alice
	// /users/Alice/role (content "admin")
	// /users/Bob
	// /users/Bob/role (content "user")

	// Check /users
	node, err := store.GetNode("users")
	require.NoError(t, err)
	assert.Contains(t, node.Children, "users/Alice")
	assert.Contains(t, node.Children, "users/Bob")

	// Check /users/Alice/role
	node, err = store.GetNode("users/Alice/role")
	require.NoError(t, err)
	assert.Equal(t, "admin", string(node.Data))
}
