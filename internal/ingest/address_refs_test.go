package ingest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/hcl"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Unit tests for ExtractAddressRefs ---

func TestExtractAddressRefs_GoOsGetenv(t *testing.T) {
	code := []byte(`package main

import "os"

func main() {
	db := os.Getenv("DATABASE_URL")
	key := os.Getenv("API_KEY")
	_ = db
	_ = key
}
`)
	lang := golang.GetLanguage()
	parser := sitter.NewParser()
	parser.SetLanguage(lang)
	tree, err := parser.ParseCtx(context.Background(), nil, code)
	require.NoError(t, err)

	w := NewSitterWalker()
	defer w.Close()

	refs, err := w.ExtractAddressRefs(tree.RootNode(), code, lang, "go")
	require.NoError(t, err)

	assert.Contains(t, refs, "env:DATABASE_URL")
	assert.Contains(t, refs, "env:API_KEY")
	assert.Len(t, refs, 2, "should find exactly two env refs")
}

func TestExtractAddressRefs_GoOsGetenv_Dedup(t *testing.T) {
	code := []byte(`package main

import "os"

func main() {
	_ = os.Getenv("SAME_VAR")
	_ = os.Getenv("SAME_VAR")
}
`)
	lang := golang.GetLanguage()
	parser := sitter.NewParser()
	parser.SetLanguage(lang)
	tree, err := parser.ParseCtx(context.Background(), nil, code)
	require.NoError(t, err)

	w := NewSitterWalker()
	defer w.Close()

	refs, err := w.ExtractAddressRefs(tree.RootNode(), code, lang, "go")
	require.NoError(t, err)

	assert.Equal(t, []string{"env:SAME_VAR"}, refs, "duplicates should be deduplicated")
}

func TestExtractAddressRefs_GoOsGetenv_NotMatched(t *testing.T) {
	code := []byte(`package main

import "fmt"

func main() {
	// Not os.Getenv -- should not match
	fmt.Println("DATABASE_URL")
}
`)
	lang := golang.GetLanguage()
	parser := sitter.NewParser()
	parser.SetLanguage(lang)
	tree, err := parser.ParseCtx(context.Background(), nil, code)
	require.NoError(t, err)

	w := NewSitterWalker()
	defer w.Close()

	refs, err := w.ExtractAddressRefs(tree.RootNode(), code, lang, "go")
	require.NoError(t, err)

	assert.Empty(t, refs, "fmt.Println should not emit env refs")
}

func TestExtractAddressRefs_GoOsGetenv_ScopeLevel(t *testing.T) {
	// Verify that extraction works on a function-body scope node,
	// not just the file root. This simulates what processNode does.
	code := []byte(`package main

import "os"

func doWork() {
	_ = os.Getenv("WORKER_DB")
}

func other() {
	_ = os.Getenv("OTHER_VAR")
}
`)
	lang := golang.GetLanguage()
	parser := sitter.NewParser()
	parser.SetLanguage(lang)
	tree, err := parser.ParseCtx(context.Background(), nil, code)
	require.NoError(t, err)

	w := NewSitterWalker()
	defer w.Close()

	// Query for function declarations to get scope nodes
	matches, err := w.Query(SitterRoot{
		Node:   tree.RootNode(),
		Source: code,
		Lang:   lang,
	}, `(function_declaration name: (identifier) @name) @scope`)
	require.NoError(t, err)
	require.Len(t, matches, 2)

	// doWork scope should find WORKER_DB
	doWorkCtx := matches[0].Context().(SitterRoot)
	refs, err := w.ExtractAddressRefs(doWorkCtx.Node, doWorkCtx.Source, doWorkCtx.Lang, "go")
	require.NoError(t, err)
	assert.Contains(t, refs, "env:WORKER_DB")
	assert.NotContains(t, refs, "env:OTHER_VAR", "other function's env var should not appear in doWork scope")

	// other scope should find OTHER_VAR
	otherCtx := matches[1].Context().(SitterRoot)
	refs2, err := w.ExtractAddressRefs(otherCtx.Node, otherCtx.Source, otherCtx.Lang, "go")
	require.NoError(t, err)
	assert.Contains(t, refs2, "env:OTHER_VAR")
	assert.NotContains(t, refs2, "env:WORKER_DB", "doWork's env var should not appear in other scope")
}

func TestExtractAddressRefs_HCLVariable(t *testing.T) {
	code := []byte(`variable "DATABASE_URL" {
  type    = string
  default = "postgres://localhost:5432/mydb"
}

variable "API_KEY" {
  type = string
}

resource "aws_instance" "web" {
  ami = "ami-12345"
}
`)
	lang := hcl.GetLanguage()
	parser := sitter.NewParser()
	parser.SetLanguage(lang)
	tree, err := parser.ParseCtx(context.Background(), nil, code)
	require.NoError(t, err)

	w := NewSitterWalker()
	defer w.Close()

	refs, err := w.ExtractAddressRefs(tree.RootNode(), code, lang, "terraform")
	require.NoError(t, err)

	assert.Contains(t, refs, "env:DATABASE_URL")
	assert.Contains(t, refs, "env:API_KEY")
	// resource blocks should NOT produce env: refs
	for _, ref := range refs {
		assert.NotContains(t, ref, "aws_instance", "resource names should not appear as env refs")
	}
	assert.Len(t, refs, 2, "should find exactly two variable env refs")
}

func TestExtractAddressRefs_NoRegistry(t *testing.T) {
	// Languages with no registered address ref queries should return nil.
	w := NewSitterWalker()
	defer w.Close()

	lang := golang.GetLanguage()
	refs, err := w.ExtractAddressRefs(nil, nil, lang, "nonexistent_lang")
	require.NoError(t, err)
	assert.Nil(t, refs, "should return nil for unregistered language")
}

func TestUnquoteCapture(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{`"DATABASE_URL"`, "DATABASE_URL"},
		{`"hello world"`, "hello world"},
		{`""`, ""},
		{`bare_token`, "bare_token"},
		{`"with\"escape"`, `with"escape`},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.expected, unquoteCapture(tt.input))
		})
	}
}

// --- Integration test: cross-language bridge via env: tokens ---

func TestEngine_AddressRefs_CrossLanguageBridge(t *testing.T) {
	// Create a mixed Go + HCL project where both reference the same env var.
	// Go code calls os.Getenv("DATABASE_URL")
	// HCL declares variable "DATABASE_URL" {}
	// Both should produce env:DATABASE_URL refs, connecting them in the graph.

	tmpDir := t.TempDir()

	goCode := []byte(`package main

import "os"

func LoadConfig() {
	db := os.Getenv("DATABASE_URL")
	_ = db
}
`)
	hclCode := []byte(`variable "DATABASE_URL" {
  type    = string
  default = "postgres://localhost:5432/mydb"
}
`)

	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "main.go"), goCode, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "variables.tf"), hclCode, 0o644))

	// Use a combined schema that handles both Go and HCL
	schemaJSON := `{
  "version": "v1",
  "nodes": [
    {
      "name": "functions",
      "selector": "$",
      "language": "go",
      "children": [
        {
          "name": "{{.name}}",
          "selector": "(function_declaration name: (identifier) @name) @scope",
          "files": [
            { "name": "source", "content_template": "{{.scope}}" }
          ]
        }
      ]
    },
    {
      "name": "variables",
      "selector": "$",
      "language": "terraform",
      "children": [
        {
          "name": "{{.name}}",
          "selector": "(block (identifier) @_type (string_lit) @name (body) @scope (#eq? @_type \"variable\"))",
          "files": [
            { "name": "source", "content_template": "{{.scope}}" }
          ]
        }
      ]
    }
  ]
}`

	var schema api.Topology
	require.NoError(t, json.Unmarshal([]byte(schemaJSON), &schema))

	store := graph.NewMemoryStore()
	engine := NewEngine(&schema, store)
	require.NoError(t, engine.Ingest(tmpDir))

	// Verify that env:DATABASE_URL connects Go and HCL constructs
	callers, err := store.GetCallers("env:DATABASE_URL")
	require.NoError(t, err)
	require.NotEmpty(t, callers, "env:DATABASE_URL should have callers from both Go and HCL")

	var goSourceFound, hclSourceFound bool
	for _, node := range callers {
		t.Logf("Caller: %s", node.ID)
		if node.ID == "functions/LoadConfig/source" {
			goSourceFound = true
		}
		// HCL variable constructs get file-level address refs
		// The exact HCL node ID depends on schema — check for any variables/ path
		if filepath.Dir(filepath.Dir(node.ID)) == "variables" {
			hclSourceFound = true
		}
	}
	assert.True(t, goSourceFound, "Go LoadConfig function should reference env:DATABASE_URL")
	assert.True(t, hclSourceFound, "HCL variable declaration should reference env:DATABASE_URL")

	// Verify the refs map contains the env: token
	refsMap := store.RefsMap()
	envRefs, ok := refsMap["env:DATABASE_URL"]
	require.True(t, ok, "env:DATABASE_URL should be in refs map")
	assert.GreaterOrEqual(t, len(envRefs), 2, "should have refs from both Go and HCL")
}
