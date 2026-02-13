package ingest

import (
	sitter "github.com/smacker/go-tree-sitter"
)

// FlattenAST walks the tree and returns a list of records for FCA analysis.
func FlattenAST(root *sitter.Node) []any {
	var records []any
	walkAST(root, &records)
	return records
}

func walkAST(n *sitter.Node, records *[]any) {
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

		*records = append(*records, rec)
	}

	// Recurse
	count := int(n.ChildCount())
	for i := 0; i < count; i++ {
		walkAST(n.Child(i), records)
	}
}
