package ingest

import (
	sitter "github.com/smacker/go-tree-sitter"

	"github.com/agentic-research/mache/internal/lang"
)

// FlattenAST walks the tree and returns a list of records for FCA analysis.
func FlattenAST(root *sitter.Node) []any {
	return FlattenASTWithLanguage(root, "")
}

// FlattenASTWithLanguage walks the tree and returns records for FCA analysis,
// using language-specific enrichment if available.
func FlattenASTWithLanguage(root *sitter.Node, langName string) []any {
	var records []any
	var enrichFn func(n *sitter.Node, rec map[string]any)
	if l := lang.ForName(langName); l != nil {
		enrichFn = l.EnrichNode
	}
	walkAST(root, &records, enrichFn)
	return records
}

func walkAST(n *sitter.Node, records *[]any, enrichFn func(*sitter.Node, map[string]any)) {
	if n == nil {
		return
	}

	// Only interest in named nodes (syntactic constructs), not anonymous tokens
	if n.IsNamed() {
		rec := make(map[string]any)
		rec["type"] = n.Type()

		// Inspect children to gather structural properties
		count := int(n.ChildCount())
		for i := 0; i < count; i++ {
			child := n.Child(i)
			if child == nil {
				continue
			}

			// Capture field names (e.g., "name", "body", "type")
			fieldName := n.FieldNameForChild(i)
			if fieldName != "" {
				// Record the presence of the field.
				// For FCA, we care about "has_name=true", "name_type=identifier"
				rec["has_"+fieldName] = true
				rec["field_"+fieldName+"_type"] = child.Type()
			}
		}

		// Apply language-specific enrichment for languages without field names
		if enrichFn != nil {
			enrichFn(n, rec)
		}

		*records = append(*records, rec)
	}

	// Recurse
	count := int(n.ChildCount())
	for i := 0; i < count; i++ {
		walkAST(n.Child(i), records, enrichFn)
	}
}
