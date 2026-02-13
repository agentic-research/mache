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

func TestSitterWalkerGo_ImportSpec(t *testing.T) {
	code := []byte(`package main

import "fmt"

import (
	"os"
	"strings"
)
`)
	root := parseSitterRoot(t, code)
	w := NewSitterWalker()

	query := `(import_spec path: (interpreted_string_literal) @path) @scope`
	matches, err := w.Query(root, query)
	require.NoError(t, err)
	require.Len(t, matches, 3)

	// Single import
	assert.Equal(t, `"fmt"`, matches[0].Values()["path"])
	assert.Contains(t, matches[0].Values()["scope"].(string), `"fmt"`)

	// Grouped imports
	assert.Equal(t, `"os"`, matches[1].Values()["path"])
	assert.Contains(t, matches[1].Values()["scope"].(string), `"os"`)

	assert.Equal(t, `"strings"`, matches[2].Values()["path"])
	assert.Contains(t, matches[2].Values()["scope"].(string), `"strings"`)

	// Each scope captures only its own import_spec
	assert.NotContains(t, matches[1].Values()["scope"].(string), "strings")
	assert.NotContains(t, matches[2].Values()["scope"].(string), "os")
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

// cobraSource is a minimal Go file with cobra.Command and flag patterns,
// used by the CLI schema query tests below.
const cobraSource = `package cmd

import "github.com/spf13/cobra"

var (
	schemaPath string
	dataPath   string
	writable   bool
)

func init() {
	rootCmd.Flags().StringVarP(&schemaPath, "schema", "s", "", "Path to topology schema")
	rootCmd.Flags().StringVarP(&dataPath, "data", "d", "", "Path to data source")
	rootCmd.Flags().BoolVarP(&writable, "writable", "w", false, "Enable write-back")
}

var rootCmd = &cobra.Command{
	Use:   "mache [mountpoint]",
	Short: "Mache: The Universal Semantic Overlay Engine",
	Args:  cobra.ExactArgs(1),
}
`

func TestSitterWalkerGo_CobraCommandQuery(t *testing.T) {
	root := parseSitterRoot(t, []byte(cobraSource))
	w := NewSitterWalker()

	query := `(var_spec
  name: (identifier) @cmd_var
  value: (expression_list
    (unary_expression
      operand: (composite_literal
        type: (qualified_type
          package: (package_identifier) @_pkg (#eq? @_pkg "cobra")
          name: (type_identifier) @_type (#eq? @_type "Command"))
        body: (literal_value) @scope))))`

	matches, err := w.Query(root, query)
	require.NoError(t, err)
	require.Len(t, matches, 1, "should find exactly one cobra.Command var")

	vals := matches[0].Values()
	assert.Equal(t, "rootCmd", vals["cmd_var"])
	assert.Equal(t, "cobra", vals["_pkg"])
	assert.Equal(t, "Command", vals["_type"])

	// Scope should contain the literal_value (struct body)
	scope, ok := vals["scope"].(string)
	require.True(t, ok)
	assert.Contains(t, scope, `"mache [mountpoint]"`)
	assert.Contains(t, scope, `"Mache: The Universal Semantic Overlay Engine"`)

	// Context should be non-nil so child queries can run against the scope
	assert.NotNil(t, matches[0].Context())
}

func TestSitterWalkerGo_CobraCommandFields(t *testing.T) {
	root := parseSitterRoot(t, []byte(cobraSource))
	w := NewSitterWalker()

	// First find the cobra.Command scope
	cmdQuery := `(var_spec
  name: (identifier) @cmd_var
  value: (expression_list
    (unary_expression
      operand: (composite_literal
        type: (qualified_type
          package: (package_identifier) @_pkg (#eq? @_pkg "cobra")
          name: (type_identifier) @_type (#eq? @_type "Command"))
        body: (literal_value) @scope))))`

	cmdMatches, err := w.Query(root, cmdQuery)
	require.NoError(t, err)
	require.Len(t, cmdMatches, 1)

	// Now query for string-valued keyed_elements within the scope
	fieldQuery := `(keyed_element
  (literal_element (identifier) @field_name)
  (literal_element (interpreted_string_literal) @value)) @scope`

	scopeCtx := cmdMatches[0].Context()
	require.NotNil(t, scopeCtx)

	fieldMatches, err := w.Query(scopeCtx, fieldQuery)
	require.NoError(t, err)
	require.Len(t, fieldMatches, 2, "should find Use and Short (Args is not a string)")

	assert.Equal(t, "Use", fieldMatches[0].Values()["field_name"])
	assert.Equal(t, `"mache [mountpoint]"`, fieldMatches[0].Values()["value"])

	assert.Equal(t, "Short", fieldMatches[1].Values()["field_name"])
	assert.Equal(t, `"Mache: The Universal Semantic Overlay Engine"`, fieldMatches[1].Values()["value"])
}

func TestSitterWalkerGo_FlagQuery(t *testing.T) {
	root := parseSitterRoot(t, []byte(cobraSource))
	w := NewSitterWalker()

	query := `(call_expression
  function: (selector_expression
    operand: (call_expression
      function: (selector_expression
        operand: (identifier) @receiver
        field: (field_identifier) @_flags_fn (#eq? @_flags_fn "Flags")))
    field: (field_identifier) @flag_method)
  arguments: (argument_list
    (_)
    (interpreted_string_literal) @flag_name
    (interpreted_string_literal) @flag_short
    (_) @flag_default
    (interpreted_string_literal) @flag_desc)) @scope`

	matches, err := w.Query(root, query)
	require.NoError(t, err)
	require.Len(t, matches, 3, "should find schema, data, writable flags")

	// Flag 1: schema (StringVarP)
	assert.Equal(t, "StringVarP", matches[0].Values()["flag_method"])
	assert.Equal(t, `"schema"`, matches[0].Values()["flag_name"])
	assert.Equal(t, `"s"`, matches[0].Values()["flag_short"])
	assert.Equal(t, `""`, matches[0].Values()["flag_default"])
	assert.Equal(t, `"Path to topology schema"`, matches[0].Values()["flag_desc"])

	// Flag 2: data (StringVarP)
	assert.Equal(t, "StringVarP", matches[1].Values()["flag_method"])
	assert.Equal(t, `"data"`, matches[1].Values()["flag_name"])
	assert.Equal(t, `"d"`, matches[1].Values()["flag_short"])

	// Flag 3: writable (BoolVarP) — non-string default
	assert.Equal(t, "BoolVarP", matches[2].Values()["flag_method"])
	assert.Equal(t, `"writable"`, matches[2].Values()["flag_name"])
	assert.Equal(t, `"w"`, matches[2].Values()["flag_short"])
	assert.Equal(t, "false", matches[2].Values()["flag_default"])
}

func TestSitterWalkerGo_PredicateFiltering(t *testing.T) {
	root := parseSitterRoot(t, []byte(cobraSource))
	w := NewSitterWalker()

	// Query for ANY var_spec with qualified_type — without predicates, this
	// would match any package.Type composite literal.
	queryNoPredicate := `(var_spec
  name: (identifier) @cmd_var
  value: (expression_list
    (unary_expression
      operand: (composite_literal
        type: (qualified_type
          package: (package_identifier) @pkg
          name: (type_identifier) @typ)
        body: (literal_value) @scope))))`

	matches, err := w.Query(root, queryNoPredicate)
	require.NoError(t, err)
	require.Len(t, matches, 1, "only one qualified-type var in test source")

	// Now with a WRONG predicate — should filter out the match
	queryWrongPredicate := `(var_spec
  name: (identifier) @cmd_var
  value: (expression_list
    (unary_expression
      operand: (composite_literal
        type: (qualified_type
          package: (package_identifier) @_pkg (#eq? @_pkg "notcobra")
          name: (type_identifier) @_type (#eq? @_type "Command"))
        body: (literal_value) @scope))))`

	matches, err = w.Query(root, queryWrongPredicate)
	require.NoError(t, err)
	assert.Len(t, matches, 0, "wrong predicate should filter out the match")
}
