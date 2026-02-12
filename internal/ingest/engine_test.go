package ingest

import (
	"encoding/json"
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

func loadGoSchema(t *testing.T) *api.Topology {
	t.Helper()
	data, err := os.ReadFile("../../examples/go-schema.json")
	require.NoError(t, err)
	var topo api.Topology
	require.NoError(t, json.Unmarshal(data, &topo))
	return &topo
}

func TestEngine_IngestTreeSitter_GoSchema(t *testing.T) {
	schema := loadGoSchema(t)

	tmpDir := t.TempDir()
	goFile := filepath.Join(tmpDir, "example.go")
	err := os.WriteFile(goFile, []byte(`package demo

const MaxRetries = 3

var DefaultName = "world"

type Greeter struct {
	Name string
}

type Speaker interface {
	Speak() string
}

func Hello() string {
	return "hello"
}

func (g *Greeter) Greet() string {
	return "Hi, " + g.Name
}

func (g Greeter) String() string {
	return g.Name
}
`), 0o644)
	require.NoError(t, err)

	store := graph.NewMemoryStore()
	engine := NewEngine(schema, store)
	require.NoError(t, engine.Ingest(goFile))

	// Package directory
	pkg, err := store.GetNode("demo")
	require.NoError(t, err)
	assert.True(t, pkg.Mode.IsDir())

	// Functions
	fnNode, err := store.GetNode("demo/functions/Hello")
	require.NoError(t, err)
	assert.True(t, fnNode.Mode.IsDir())

	fnSource, err := store.GetNode("demo/functions/Hello/source")
	require.NoError(t, err)
	assert.Contains(t, string(fnSource.Data), "func Hello()")

	// Methods — pointer receiver
	methodNode, err := store.GetNode("demo/methods/Greeter.Greet")
	require.NoError(t, err)
	assert.True(t, methodNode.Mode.IsDir())

	methodSource, err := store.GetNode("demo/methods/Greeter.Greet/source")
	require.NoError(t, err)
	assert.Contains(t, string(methodSource.Data), "func (g *Greeter) Greet()")

	// Methods — value receiver
	valMethodSource, err := store.GetNode("demo/methods/Greeter.String/source")
	require.NoError(t, err)
	assert.Contains(t, string(valMethodSource.Data), "func (g Greeter) String()")

	// Types
	typeNode, err := store.GetNode("demo/types/Greeter")
	require.NoError(t, err)
	assert.True(t, typeNode.Mode.IsDir())

	typeSource, err := store.GetNode("demo/types/Greeter/source")
	require.NoError(t, err)
	assert.Contains(t, string(typeSource.Data), "Greeter struct")

	// Interface type
	ifaceSource, err := store.GetNode("demo/types/Speaker/source")
	require.NoError(t, err)
	assert.Contains(t, string(ifaceSource.Data), "Speaker interface")

	// Constants
	constSource, err := store.GetNode("demo/constants/MaxRetries/source")
	require.NoError(t, err)
	assert.Contains(t, string(constSource.Data), "MaxRetries")

	// Variables
	varSource, err := store.GetNode("demo/variables/DefaultName/source")
	require.NoError(t, err)
	assert.Contains(t, string(varSource.Data), "DefaultName")
}

func TestEngine_IngestTreeSitter_MultiFileMerge(t *testing.T) {
	schema := loadGoSchema(t)

	tmpDir := t.TempDir()

	// First file in package
	err := os.WriteFile(filepath.Join(tmpDir, "a.go"), []byte(`package shared

func FuncA() {}

type TypeA struct{}
`), 0o644)
	require.NoError(t, err)

	// Second file in same package
	err = os.WriteFile(filepath.Join(tmpDir, "b.go"), []byte(`package shared

func FuncB() {}

type TypeB struct{}
`), 0o644)
	require.NoError(t, err)

	store := graph.NewMemoryStore()
	engine := NewEngine(schema, store)
	require.NoError(t, engine.Ingest(tmpDir))

	// Both files contribute to the same package directory
	pkg, err := store.GetNode("shared")
	require.NoError(t, err)
	assert.True(t, pkg.Mode.IsDir())

	// Functions from both files
	_, err = store.GetNode("shared/functions/FuncA/source")
	require.NoError(t, err)
	_, err = store.GetNode("shared/functions/FuncB/source")
	require.NoError(t, err)

	// Types from both files
	_, err = store.GetNode("shared/types/TypeA/source")
	require.NoError(t, err)
	_, err = store.GetNode("shared/types/TypeB/source")
	require.NoError(t, err)

	// Functions dir contains both
	fns, err := store.GetNode("shared/functions")
	require.NoError(t, err)
	assert.Contains(t, fns.Children, "shared/functions/FuncA")
	assert.Contains(t, fns.Children, "shared/functions/FuncB")
}

func TestEngine_IngestTreeSitter_GroupedDeclarations(t *testing.T) {
	schema := loadGoSchema(t)

	tmpDir := t.TempDir()
	goFile := filepath.Join(tmpDir, "grouped.go")
	err := os.WriteFile(goFile, []byte(`package grouped

type (
	Alpha struct {
		Name string
	}

	Beta interface {
		Run()
	}
)

const (
	ConstX = 1
	ConstY = 2
)

var (
	VarA = "a"
	VarB = "b"
)
`), 0o644)
	require.NoError(t, err)

	store := graph.NewMemoryStore()
	engine := NewEngine(schema, store)
	require.NoError(t, engine.Ingest(goFile))

	// Grouped types: each gets its own source, not the whole block
	alphaSource, err := store.GetNode("grouped/types/Alpha/source")
	require.NoError(t, err)
	assert.Contains(t, string(alphaSource.Data), "Alpha struct")
	assert.NotContains(t, string(alphaSource.Data), "Beta interface")

	betaSource, err := store.GetNode("grouped/types/Beta/source")
	require.NoError(t, err)
	assert.Contains(t, string(betaSource.Data), "Beta interface")
	assert.NotContains(t, string(betaSource.Data), "Alpha struct")

	// Grouped constants: each const_spec is isolated
	cxSource, err := store.GetNode("grouped/constants/ConstX/source")
	require.NoError(t, err)
	assert.Contains(t, string(cxSource.Data), "ConstX")
	assert.NotContains(t, string(cxSource.Data), "ConstY")

	// Grouped variables: each var_spec is isolated
	vaSource, err := store.GetNode("grouped/variables/VarA/source")
	require.NoError(t, err)
	assert.Contains(t, string(vaSource.Data), "VarA")
	assert.NotContains(t, string(vaSource.Data), "VarB")
}
