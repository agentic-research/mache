package ingest

import (
	"context"
	"testing"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSitterWalkerPython(t *testing.T) {
	code := []byte(`
def hello():
    print("world")

def add(a, b):
    return a + b
`)
	// Create parser
	lang := python.GetLanguage()
	parser := sitter.NewParser()
	parser.SetLanguage(lang)

	// Parse code
	tree, err := parser.ParseCtx(context.Background(), nil, code)
	require.NoError(t, err)

	w := NewSitterWalker()

	// Query for function definitions
	// This query captures the function name identifier as @name
	query := `(function_definition name: (identifier) @name)`

	// Prepare root context
	root := SitterRoot{
		Node:   tree.RootNode(),
		Source: code,
		Lang:   lang, // Query needs language
	}

	matches, err := w.Query(root, query)
	require.NoError(t, err)

	assert.Len(t, matches, 2)

	// Verify captures
	assert.Equal(t, map[string]any{"name": "hello"}, matches[0].Values())
	assert.Equal(t, map[string]any{"name": "add"}, matches[1].Values())
}

func TestSitterWalkerGo(t *testing.T) {
	code := []byte(`
package main

func main() {
	println("hello")
}

func helper() {}
`)
	lang := golang.GetLanguage()
	parser := sitter.NewParser()
	parser.SetLanguage(lang)

	tree, err := parser.ParseCtx(context.Background(), nil, code)
	require.NoError(t, err)

	w := NewSitterWalker()

	// Query for function declarations
	query := `(function_declaration name: (identifier) @name)`

	root := SitterRoot{
		Node:   tree.RootNode(),
		Source: code,
		Lang:   lang,
	}

	matches, err := w.Query(root, query)
	require.NoError(t, err)

	assert.Len(t, matches, 2)
	assert.Equal(t, map[string]any{"name": "main"}, matches[0].Values())
	assert.Equal(t, map[string]any{"name": "helper"}, matches[1].Values())
}

// parseSitterRoot is a test helper that parses Go source and returns a SitterRoot.
func parseSitterRoot(t *testing.T, code []byte) SitterRoot {
	t.Helper()
	lang := golang.GetLanguage()
	parser := sitter.NewParser()
	parser.SetLanguage(lang)
	tree, err := parser.ParseCtx(context.Background(), nil, code)
	require.NoError(t, err)
	return SitterRoot{Node: tree.RootNode(), Source: code, Lang: lang}
}

func TestSitterWalkerGo_DollarRootSelector(t *testing.T) {
	code := []byte(`package main

func hello() {}
`)
	root := parseSitterRoot(t, code)
	w := NewSitterWalker()

	matches, err := w.Query(root, "$")
	require.NoError(t, err)
	require.Len(t, matches, 1)

	// "$" returns empty values (literal names don't need captures)
	assert.Empty(t, matches[0].Values())

	// Context is non-nil and can be used for child queries
	ctx := matches[0].Context()
	require.NotNil(t, ctx)

	// Child query against the context should find the function
	childMatches, err := w.Query(ctx, `(function_declaration name: (identifier) @name) @scope`)
	require.NoError(t, err)
	require.Len(t, childMatches, 1)
	assert.Equal(t, "hello", childMatches[0].Values()["name"])
}

func TestSitterWalkerGo_PackageClause(t *testing.T) {
	code := []byte(`package mypackage

func Foo() {}
`)
	root := parseSitterRoot(t, code)
	w := NewSitterWalker()

	matches, err := w.Query(root, `(source_file (package_clause (package_identifier) @pkg) @scope)`)
	require.NoError(t, err)
	require.Len(t, matches, 1)
	assert.Equal(t, "mypackage", matches[0].Values()["pkg"])

	// Scope context is non-nil (the package_clause node)
	ctx := matches[0].Context()
	assert.NotNil(t, ctx)
}

func TestSitterWalkerGo_Methods(t *testing.T) {
	code := []byte(`package foo

type MyStruct struct{}

func (m *MyStruct) PointerMethod() {}
func (m MyStruct) ValueMethod() {}
`)
	root := parseSitterRoot(t, code)
	w := NewSitterWalker()

	// Query for methods with pointer receiver
	pointerQuery := `(method_declaration receiver: (parameter_list (parameter_declaration type: (pointer_type (type_identifier) @receiver))) name: (field_identifier) @name) @scope`
	matches, err := w.Query(root, pointerQuery)
	require.NoError(t, err)
	require.Len(t, matches, 1)
	assert.Equal(t, "MyStruct", matches[0].Values()["receiver"])
	assert.Equal(t, "PointerMethod", matches[0].Values()["name"])

	// Query for methods with value receiver
	valueQuery := `(method_declaration receiver: (parameter_list (parameter_declaration type: (type_identifier) @receiver)) name: (field_identifier) @name) @scope`
	matches, err = w.Query(root, valueQuery)
	require.NoError(t, err)
	require.Len(t, matches, 1)
	assert.Equal(t, "MyStruct", matches[0].Values()["receiver"])
	assert.Equal(t, "ValueMethod", matches[0].Values()["name"])
}

func TestSitterWalkerGo_Types(t *testing.T) {
	code := []byte(`package foo

type MyStruct struct {
	Name string
}

type MyInterface interface {
	DoSomething()
}
`)
	root := parseSitterRoot(t, code)
	w := NewSitterWalker()

	query := `(type_declaration (type_spec name: (type_identifier) @name) @scope)`
	matches, err := w.Query(root, query)
	require.NoError(t, err)
	require.Len(t, matches, 2)
	assert.Equal(t, "MyStruct", matches[0].Values()["name"])
	assert.Equal(t, "MyInterface", matches[1].Values()["name"])

	// @scope on type_spec captures just the spec, not the whole declaration
	structSource := matches[0].Values()["scope"].(string)
	assert.Contains(t, structSource, "MyStruct struct")
	assert.NotContains(t, structSource, "MyInterface")
}

func TestSitterWalkerGo_GroupedTypes(t *testing.T) {
	code := []byte(`package foo

type (
	Grouped1 struct {
		X int
	}

	Grouped2 interface {
		Method()
	}
)
`)
	root := parseSitterRoot(t, code)
	w := NewSitterWalker()

	query := `(type_declaration (type_spec name: (type_identifier) @name) @scope)`
	matches, err := w.Query(root, query)
	require.NoError(t, err)
	require.Len(t, matches, 2)

	// Each match captures only its own type_spec, not the whole grouped block
	g1Source := matches[0].Values()["scope"].(string)
	assert.Contains(t, g1Source, "Grouped1 struct")
	assert.NotContains(t, g1Source, "Grouped2")

	g2Source := matches[1].Values()["scope"].(string)
	assert.Contains(t, g2Source, "Grouped2 interface")
	assert.NotContains(t, g2Source, "Grouped1")
}

func TestSitterWalkerGo_GroupedConstants(t *testing.T) {
	code := []byte(`package foo

const (
	A = "alpha"
	B = "beta"
)
`)
	root := parseSitterRoot(t, code)
	w := NewSitterWalker()

	query := `(const_spec name: (identifier) @name) @scope`
	matches, err := w.Query(root, query)
	require.NoError(t, err)
	require.Len(t, matches, 2)

	// Each const_spec captures only its own line, not the whole block
	aSource := matches[0].Values()["scope"].(string)
	assert.Contains(t, aSource, "A")
	assert.NotContains(t, aSource, "B")

	bSource := matches[1].Values()["scope"].(string)
	assert.Contains(t, bSource, "B")
	assert.NotContains(t, bSource, "A")
}

func TestSitterWalkerGo_ConstantsAndVariables(t *testing.T) {
	code := []byte(`package foo

const SingleConst = 42

const (
	GroupedA = "a"
	GroupedB = "b"
)

var SingleVar = "hello"

var (
	GroupedVarX = 1
	GroupedVarY = 2
)
`)
	root := parseSitterRoot(t, code)
	w := NewSitterWalker()

	// Constants
	constQuery := `(const_spec name: (identifier) @name) @scope`
	matches, err := w.Query(root, constQuery)
	require.NoError(t, err)
	require.Len(t, matches, 3)
	assert.Equal(t, "SingleConst", matches[0].Values()["name"])
	assert.Equal(t, "GroupedA", matches[1].Values()["name"])
	assert.Equal(t, "GroupedB", matches[2].Values()["name"])

	// Variables
	varQuery := `(var_spec name: (identifier) @name) @scope`
	matches, err = w.Query(root, varQuery)
	require.NoError(t, err)
	require.Len(t, matches, 3)
	assert.Equal(t, "SingleVar", matches[0].Values()["name"])
	assert.Equal(t, "GroupedVarX", matches[1].Values()["name"])
	assert.Equal(t, "GroupedVarY", matches[2].Values()["name"])
}
