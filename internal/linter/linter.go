package linter

import (
	"context"
	"fmt"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"
)

type Diagnostic struct {
	Message string
	Line    uint32
}

func (d Diagnostic) String() string {
	return fmt.Sprintf("line %d: %s", d.Line+1, d.Message)
}

// Lint checks the content for static analysis issues.
// Currently supports Go only.
func Lint(content []byte, lang string) ([]Diagnostic, error) {
	if lang != "go" && !strings.HasSuffix(lang, ".go") {
		return nil, nil
	}

	parser := sitter.NewParser()
	parser.SetLanguage(golang.GetLanguage())
	tree, err := parser.ParseCtx(context.Background(), nil, content)
	if err != nil {
		return nil, err
	}

	var diags []Diagnostic

	// Rule 1: Nil Slice declaration
	// var x []T (without initialization)
	// Query: (var_declaration (var_spec name: (identifier) type: (slice_type) !value))
	// Tree-sitter query syntax for "missing field" is slightly different depending on grammar.
	// For Go: var_spec has 'name', 'type', 'value'.
	// If 'value' is missing, it's a nil slice.

	// Query to find slice declarations without values
	query := `
		(var_declaration
			(var_spec
				name: (identifier)
				type: (slice_type)
			) @decl
		)
	`
	q, _ := sitter.NewQuery([]byte(query), golang.GetLanguage())
	qc := sitter.NewQueryCursor()
	qc.Exec(q, tree.RootNode())

	for {
		m, ok := qc.NextMatch()
		if !ok {
			break
		}
		for _, c := range m.Captures {
			// Check if the var_spec has a value child
			// The query finds var_spec with name and type.
			// We need to verify it does NOT have a value.
			// var_spec structure: name, type, [value]

			// Simple check: iterate children of var_spec
			hasValue := false
			count := int(c.Node.ChildCount())
			for i := 0; i < count; i++ {
				// In Go grammar, the value is usually an expression list?
				// Field names: name, type, value
				if c.Node.FieldNameForChild(i) == "value" {
					hasValue = true
					break
				}
			}

			if !hasValue {
				diags = append(diags, Diagnostic{
					Message: "Nil slice declaration. Consider 'make([]T, 0)' for JSON compatibility.",
					Line:    c.Node.StartPoint().Row,
				})
			}
		}
	}

	return diags, nil
}
