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
	// Setup Schema
	schema := &api.Topology{
		Version: "v1",
		Nodes: []api.Node{
			{
				Name:     "users",
				Selector: "$.users[*]",
				Children: []api.Node{
					{
						Name:     "{{.name}}",
						Selector: "@", // Select self as context for children?
						// Wait, current logic:
						// processNode(users) -> Query($.users[*]) -> Matches (user objects)
						// -> Create dir "users/{{.name}}" (Wait, Name is just "users"?)
						// Schema says Name: "users".
						// If Selector returns multiple matches, do we create multiple "users" dirs?
						// Yes, logic: for match in matches: create dir named schema.Name.
						// So if Name="users", we create "users", "users"... collisions!
						// Usually the top level node selects a list, but we want a parent dir "users" containing children?
						// Or we want "users" dir to contain user dirs?
						
						// User Prompt Example:
						// Selector (resource_declaration ... )
						// Path = "/{{.type}}/{{.name}}"
						// So the Node Name template uses captures.
					},
				},
			},
		},
	}
	
	// Let's adjust schema to match logic.
	// We want /users/Alice, /users/Bob.
	// So we need a root node "users" (static) -> selector selects the list?
	// If selector selects list, match is the list.
	// If selector selects items, match is item.
	
	// If we want /users/Alice:
	// Node 1: Name="users", Selector="$.users" (matches the array? or just static?)
	// If selector is empty, query returns root?
	// If we select $.users, we get [array].
	// Then children?
	
	// Let's try:
	// Node 1: Name="users", Selector="" (or "$"). Match root.
	//   Children: Node 2: Name="{{.name}}", Selector="$.users[*]"
	
	// Let's refine the test schema.
	schema = &api.Topology{
		Nodes: []api.Node{
			{
				Name: "users",
				Selector: "$", // Root object
				Children: []api.Node{
					{
						Name: "{{.name}}",
						Selector: "users[*]", // Relative to root
						Files: []api.Leaf{
							{
								Name: "role",
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
`), 0644)
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
