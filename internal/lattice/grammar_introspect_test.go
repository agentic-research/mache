package lattice

import (
	"context"
	"testing"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/hcl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/ingest"
)

// TestDiscoverHCLNodeTypes enumerates all node types from HCL grammar
func TestDiscoverHCLNodeTypes(t *testing.T) {
	lang := hcl.GetLanguage()

	var types []string
	for i := uint32(0); i < lang.SymbolCount(); i++ {
		sym := sitter.Symbol(i)
		if lang.SymbolType(sym) == sitter.SymbolTypeRegular {
			name := lang.SymbolName(sym)
			types = append(types, name)
		}
	}

	// Verify we can enumerate types
	require.NotEmpty(t, types)
	t.Logf("HCL has %d node types", len(types))

	// Check for expected HCL types
	assert.Contains(t, types, "block")
	assert.Contains(t, types, "identifier")
	assert.Contains(t, types, "body")

	// Find definition candidates using heuristics
	patterns := []string{"_block", "_declaration", "_definition", "_statement", "_call"}
	var candidates []string

	for _, typ := range types {
		for _, pat := range patterns {
			if contains(typ, pat) && !contains(typ, "ERROR") {
				candidates = append(candidates, typ)
				break
			}
		}
	}

	t.Logf("HCL definition candidates: %v", candidates)
}

// TestDiscoverHCLAttributes is the critical test - it parses sample Terraform
// and logs what attributes FlattenAST generates for blocks
func TestDiscoverHCLAttributes(t *testing.T) {
	sample := `
resource "aws_s3_bucket" "my_bucket" {
  bucket = "test-bucket"
  acl    = "private"
}

module "vpc" {
  source = "./modules/vpc"
  cidr   = "10.0.0.0/16"
}

data "aws_caller_identity" "current" {}

variable "region" {
  type    = string
  default = "us-east-1"
}
`
	lang := hcl.GetLanguage()
	parser := sitter.NewParser()
	parser.SetLanguage(lang)
	tree, err := parser.ParseCtx(context.Background(), nil, []byte(sample))
	require.NoError(t, err)

	// Use the language-aware FlattenAST for HCL
	records := ingest.FlattenASTWithLanguage(tree.RootNode(), "terraform")

	t.Logf("Total records from FlattenASTWithLanguage: %d", len(records))

	// Group records by type to understand structure
	typeStats := make(map[string]int)
	blockRecords := []map[string]any{}

	for _, rec := range records {
		recMap, ok := rec.(map[string]any)
		if !ok {
			continue
		}

		typ, ok := recMap["type"].(string)
		if !ok {
			continue
		}

		typeStats[typ]++

		// Collect block records for detailed inspection
		if typ == "block" {
			blockRecords = append(blockRecords, recMap)
		}
	}

	t.Logf("Record type distribution:")
	for typ, count := range typeStats {
		t.Logf("  %s: %d", typ, count)
	}

	t.Logf("\nDetailed block attributes:")
	for i, block := range blockRecords {
		t.Logf("\nBlock %d:", i+1)
		for key, val := range block {
			// Skip large fields
			if key == "text" || key == "source" {
				t.Logf("  %s: <omitted>", key)
			} else {
				t.Logf("  %s: %v", key, val)
			}
		}
	}

	// Check for specific attribute patterns that ProjectAST looks for
	t.Logf("\nAttribute pattern analysis:")
	for i, block := range blockRecords {
		hasName := false
		hasBody := false
		nameAttrPattern := ""
		bodyAttrPattern := ""

		for key := range block {
			if key == "has_name" || hasPrefix(key, "field_name_type=") {
				hasName = true
				nameAttrPattern = key
			}
			if key == "has_body" || hasPrefix(key, "field_body_type=") {
				hasBody = true
				bodyAttrPattern = key
			}
		}

		t.Logf("Block %d: hasName=%v (%s), hasBody=%v (%s)",
			i+1, hasName, nameAttrPattern, hasBody, bodyAttrPattern)
	}
}

// TestFlattenHCLResourceBlock focuses on a single resource to see exact structure
func TestFlattenHCLResourceBlock(t *testing.T) {
	sample := `resource "aws_s3_bucket" "my_bucket" {
  bucket = "test-bucket"
}`

	lang := hcl.GetLanguage()
	parser := sitter.NewParser()
	parser.SetLanguage(lang)
	tree, err := parser.ParseCtx(context.Background(), nil, []byte(sample))
	require.NoError(t, err)

	records := ingest.FlattenAST(tree.RootNode())

	t.Logf("Records from single resource block:")
	for i, rec := range records {
		recMap, ok := rec.(map[string]any)
		if !ok {
			continue
		}

		t.Logf("\nRecord %d:", i+1)
		for key, val := range recMap {
			if key == "text" || key == "source" {
				continue
			}
			t.Logf("  %s: %v", key, val)
		}
	}
}

// TestInspectHCLFieldNames directly inspects tree-sitter nodes to see field names
func TestInspectHCLFieldNames(t *testing.T) {
	sample := `resource "aws_s3_bucket" "my_bucket" {
  bucket = "test-bucket"
}

module "vpc" {
  source = "./vpc"
}

data "aws_ami" "ubuntu" {
  most_recent = true
}`

	lang := hcl.GetLanguage()
	parser := sitter.NewParser()
	parser.SetLanguage(lang)
	tree, err := parser.ParseCtx(context.Background(), nil, []byte(sample))
	require.NoError(t, err)

	t.Logf("\nInspecting HCL tree structure:")
	inspectNode(tree.RootNode(), 0, t)
}

func inspectNode(n *sitter.Node, depth int, t *testing.T) {
	if n == nil {
		return
	}

	indent := ""
	for i := 0; i < depth; i++ {
		indent += "  "
	}

	if n.IsNamed() {
		t.Logf("%s%s", indent, n.Type())

		// Show field names for children
		count := int(n.ChildCount())
		for i := 0; i < count; i++ {
			child := n.Child(i)
			if child == nil {
				continue
			}

			fieldName := n.FieldNameForChild(i)
			if fieldName != "" && child.IsNamed() {
				t.Logf("%s  field '%s': %s", indent, fieldName, child.Type())
			}
		}

		// Only recurse for block types to keep output manageable
		if n.Type() == "block" || n.Type() == "config_file" || n.Type() == "body" {
			for i := 0; i < count; i++ {
				inspectNode(n.Child(i), depth+1, t)
			}
		}
	}
}

// TestHCLFCAInference tests the full FCA inference pipeline for HCL
func TestHCLFCAInference(t *testing.T) {
	sample := `
resource "aws_s3_bucket" "my_bucket" {
  bucket = "test-bucket"
}

module "vpc" {
  source = "./vpc"
}

data "aws_ami" "ubuntu" {
  most_recent = true
}
`

	lang := hcl.GetLanguage()
	parser := sitter.NewParser()
	parser.SetLanguage(lang)
	tree, err := parser.ParseCtx(context.Background(), nil, []byte(sample))
	require.NoError(t, err)

	// Flatten with language awareness
	records := ingest.FlattenASTWithLanguage(tree.RootNode(), "terraform")
	t.Logf("Generated %d records from HCL sample", len(records))

	// Run FCA inference
	inf := &Inferrer{
		Config: InferConfig{
			Method:   "fca",
			Language: "terraform",
		},
	}

	topology, err := inf.InferFromRecords(records)
	require.NoError(t, err)

	t.Logf("Inferred topology:")
	t.Logf("  Version: %s", topology.Version)
	t.Logf("  Nodes: %d", len(topology.Nodes))

	for i, node := range topology.Nodes {
		t.Logf("  Node %d:", i)
		t.Logf("    Name: %s", node.Name)
		t.Logf("    Selector: %s", node.Selector)
		t.Logf("    Language: %s", node.Language)
		t.Logf("    Children: %d", len(node.Children))
		t.Logf("    Files: %d", len(node.Files))

		if len(node.Children) > 0 {
			for j, child := range node.Children {
				t.Logf("      Child %d: name=%s, selector=%s", j, child.Name, child.Selector)
			}
		}
	}

	// Verify we got a non-empty schema
	assert.NotEmpty(t, topology.Nodes, "HCL should produce a non-empty schema")
}

// TestHCLMultiLanguageInference tests InferMultiLanguage like the agent mode does
func TestHCLMultiLanguageInference(t *testing.T) {
	sample := `
resource "aws_s3_bucket" "my_bucket" {
  bucket = "test-bucket"
}

module "vpc" {
  source = "./vpc"
}
`

	lang := hcl.GetLanguage()
	parser := sitter.NewParser()
	parser.SetLanguage(lang)
	tree, err := parser.ParseCtx(context.Background(), nil, []byte(sample))
	require.NoError(t, err)

	// Flatten with language awareness
	records := ingest.FlattenASTWithLanguage(tree.RootNode(), "terraform")

	// Create recordsByLang map like agent mode does
	recordsByLang := map[string][]any{
		"terraform": records,
	}

	// Call InferMultiLanguage like agent mode does
	inf := &Inferrer{
		Config: InferConfig{
			Method: "fca",
		},
	}

	topology, err := inf.InferMultiLanguage(recordsByLang)
	require.NoError(t, err)

	t.Logf("Multi-language topology:")
	t.Logf("  Version: %s", topology.Version)
	t.Logf("  Nodes: %d", len(topology.Nodes))

	for i, node := range topology.Nodes {
		t.Logf("  Node %d: name=%s, language=%s, children=%d",
			i, node.Name, node.Language, len(node.Children))

		if len(node.Children) > 0 {
			for j, child := range node.Children {
				t.Logf("    Child %d: name=%s", j, child.Name)
			}
		}
	}

	// Verify terraform namespace exists
	assert.NotEmpty(t, topology.Nodes, "Should have language namespaces")

	// Find terraform node
	var tfNode *api.Node
	for i := range topology.Nodes {
		if topology.Nodes[i].Name == "terraform" {
			tfNode = &topology.Nodes[i]
			break
		}
	}

	require.NotNil(t, tfNode, "terraform namespace should exist")
	assert.NotEmpty(t, tfNode.Children, "terraform should have children")
}

// Helper functions
func contains(s, substr string) bool {
	return len(s) >= len(substr) && findSubstring(s, substr) >= 0
}

func findSubstring(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
