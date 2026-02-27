package ingest

import (
	"context"
	"testing"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/hcl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFlattenAST_Go(t *testing.T) {
	code := []byte(`func Hello() {}`)
	lang := golang.GetLanguage()
	parser := sitter.NewParser()
	parser.SetLanguage(lang)

	tree, err := parser.ParseCtx(context.Background(), nil, code)
	require.NoError(t, err)

	records := FlattenAST(tree.RootNode())
	require.NotEmpty(t, records)

	found := false
	for _, r := range records {
		rec := r.(map[string]any)
		if rec["type"] == "function_declaration" {
			assert.Equal(t, true, rec["has_name"])
			assert.Equal(t, "identifier", rec["field_name_type"])
			found = true
			break
		}
	}
	assert.True(t, found, "should have found function_declaration record")
}

func TestFlattenAST_HCL(t *testing.T) {
	code := []byte(`resource "a" "b" {}`)
	lang := hcl.GetLanguage()
	parser := sitter.NewParser()
	parser.SetLanguage(lang)

	tree, err := parser.ParseCtx(context.Background(), nil, code)
	require.NoError(t, err)

	records := FlattenASTWithLanguage(tree.RootNode(), "hcl")
	require.NotEmpty(t, records)

	// Verify we get records with type information from the HCL AST
	hasBlock := false
	for _, r := range records {
		rec := r.(map[string]any)
		if rec["type"] == "block" {
			hasBlock = true
			break
		}
	}
	assert.True(t, hasBlock, "should have found block record in HCL AST")
}

func TestFlattenAST_Nil(t *testing.T) {
	records := FlattenAST(nil)
	assert.Empty(t, records)
}
