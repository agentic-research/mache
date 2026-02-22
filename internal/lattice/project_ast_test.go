package lattice

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildGoContext creates a FormalContext simulating Go source with
// function_definition (has_name, has_body) — the typical Go FCA output.
func buildGoContext() (*FormalContext, []Concept) {
	attrs := []string{
		"type=function_definition",
		"has_name",
		"has_body",
		"field_name_type=identifier",
	}
	// Two Go function objects, both have all attributes
	incidence := [][]bool{
		{true, true, true, true},
		{true, true, true, true},
	}
	ctx := NewFormalContext(2, attrs, incidence)
	concepts := NextClosure(ctx)
	return ctx, concepts
}

// buildPythonContext creates a FormalContext simulating Python source with
// function_definition and class_definition (both have name+body).
func buildPythonContext() (*FormalContext, []Concept) {
	attrs := []string{
		"has_name",
		"has_body",
		"field_name_type=identifier",
		"type=function_definition",
		"type=class_definition",
	}
	incidence := [][]bool{
		// func0: function_definition with name+body
		{true, true, true, true, false},
		// class0: class_definition with name+body
		{true, true, true, false, true},
	}
	ctx := NewFormalContext(2, attrs, incidence)
	concepts := NextClosure(ctx)
	return ctx, concepts
}

func TestProjectAST_GoFlat(t *testing.T) {
	ctx, concepts := buildGoContext()

	topo := ProjectAST(concepts, ctx, ProjectConfig{Language: "go"})

	require.NotNil(t, topo)
	// Go has function_definition → 1 group node ("functions")
	require.Len(t, topo.Nodes, 1)

	group := topo.Nodes[0]
	assert.Equal(t, "functions", group.Name)
	assert.Equal(t, "$", group.Selector)
	require.Len(t, group.Children, 1)

	funcNode := group.Children[0]
	assert.Equal(t, "{{.name}}", funcNode.Name)
	assert.Contains(t, funcNode.Selector, "function_definition")
	// Go is flat — no nested children
	assert.Empty(t, funcNode.Children, "Go functions should have no children (flat)")
}

func TestProjectAST_PythonNested(t *testing.T) {
	ctx, concepts := buildPythonContext()

	topo := ProjectAST(concepts, ctx, ProjectConfig{Language: "python"})

	require.NotNil(t, topo)
	// Python has class_definition + function_definition → 2 groups ("classes", "functions")
	require.Len(t, topo.Nodes, 2)

	// Groups are ordered by first appearance of sorted types:
	// sortedTypes = [class_definition, function_definition] → groups = [classes, functions]
	classGroup := topo.Nodes[0]
	funcGroup := topo.Nodes[1]
	assert.Equal(t, "classes", classGroup.Name)
	assert.Equal(t, "$", classGroup.Selector)
	assert.Equal(t, "functions", funcGroup.Name)
	assert.Equal(t, "$", funcGroup.Selector)

	require.Len(t, classGroup.Children, 1)
	require.Len(t, funcGroup.Children, 1)

	classNode := classGroup.Children[0]
	funcNode := funcGroup.Children[0]
	assert.Contains(t, classNode.Selector, "class_definition")
	assert.Contains(t, funcNode.Selector, "function_definition")

	// class_definition should have function_definition as nested child
	require.Len(t, classNode.Children, 1, "Python class should nest function_definition")
	assert.Contains(t, classNode.Children[0].Selector, "function_definition")

	// function_definition should be flat (no self-nesting)
	assert.Empty(t, funcNode.Children, "Python functions should not nest")
}

func TestProjectAST_UnknownLanguageFlat(t *testing.T) {
	ctx, concepts := buildPythonContext()

	// Unknown language should default to flat (safe fallback)
	topo := ProjectAST(concepts, ctx, ProjectConfig{Language: "cobol"})

	require.NotNil(t, topo)
	// Still grouped: "classes" and "functions"
	require.Len(t, topo.Nodes, 2)

	for _, group := range topo.Nodes {
		assert.Equal(t, "$", group.Selector, "Group should use $ passthrough selector")
		require.Len(t, group.Children, 1)
		assert.Empty(t, group.Children[0].Children, "Unknown language should produce flat schema")
	}
}

func TestProjectAST_EmptyLanguageFlat(t *testing.T) {
	ctx, concepts := buildPythonContext()

	// Empty language string should also be flat
	topo := ProjectAST(concepts, ctx, ProjectConfig{Language: ""})

	require.NotNil(t, topo)
	for _, group := range topo.Nodes {
		assert.Equal(t, "$", group.Selector, "Group should use $ passthrough selector")
		require.Len(t, group.Children, 1)
		assert.Empty(t, group.Children[0].Children, "Empty language should produce flat schema")
	}
}
