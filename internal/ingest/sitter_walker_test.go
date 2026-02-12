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
