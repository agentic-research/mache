package ingest

import (
	"strings"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/internal/lang"
	sitter "github.com/smacker/go-tree-sitter"
)

// langForExt is a thin wrapper over the lang registry.
// Returns nil, "" for unsupported extensions.
func langForExt(ext string) (*sitter.Language, string) {
	l := lang.ForExt(ext)
	if l == nil {
		return nil, ""
	}
	return l.Grammar(), l.Name
}

// SchemaUsesTreeSitter returns true if the schema's selectors are tree-sitter
// S-expressions rather than JSONPath. S-expressions always start with '('.
func SchemaUsesTreeSitter(schema *api.Topology) bool {
	return hasTreeSitterSelectors(schema.Nodes)
}

// hasTreeSitterSelectors recursively checks for tree-sitter S-expression selectors.
func hasTreeSitterSelectors(nodes []api.Node) bool {
	for _, n := range nodes {
		sel := strings.TrimSpace(n.Selector)
		if len(sel) > 0 && sel[0] == '(' {
			return true
		}
		if hasTreeSitterSelectors(n.Children) {
			return true
		}
	}
	return false
}

// filterNodesByLanguage returns nodes that match the given language.
// Nodes match if:
// - Their Language field equals langName (FCA-generated nodes with language tags)
// - Their Name equals langName (namespace nodes from multi-language inference)
// - Their Language field is empty (manual schemas, tests, language-agnostic nodes)
func filterNodesByLanguage(nodes []api.Node, langName string) []api.Node {
	var result []api.Node
	for _, node := range nodes {
		if node.Language == langName || node.Name == langName || node.Language == "" {
			result = append(result, node)
		}
	}
	return result
}
