package writeback

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/python"
	sqllang "github.com/smacker/go-tree-sitter/sql"
	"github.com/smacker/go-tree-sitter/typescript/typescript"
)

// ValidationError contains structured information about a syntax error.
type ValidationError struct {
	FilePath string
	Line     uint32 // 0-indexed
	Column   uint32 // 0-indexed
	Message  string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s:%d:%d: %s", e.FilePath, e.Line+1, e.Column+1, e.Message)
}

// Validate parses content with tree-sitter and returns an error if the AST
// contains syntax errors. Files with no known tree-sitter language pass
// through without validation (returns nil).
func Validate(content []byte, filePath string) error {
	lang := languageForPath(filePath)
	if lang == nil {
		return nil // unknown language — pass through
	}

	parser := sitter.NewParser()
	parser.SetLanguage(lang)

	tree, err := parser.ParseCtx(context.Background(), nil, content)
	if err != nil {
		return fmt.Errorf("tree-sitter parse failed for %s: %w", filePath, err)
	}

	root := tree.RootNode()
	if root == nil {
		return fmt.Errorf("tree-sitter returned nil root for %s", filePath)
	}

	if !root.HasError() {
		return nil
	}

	// Walk tree to find first ERROR node for a useful error message
	errNode := findFirstError(root)
	if errNode != nil {
		return &ValidationError{
			FilePath: filePath,
			Line:     uint32(errNode.StartPoint().Row),
			Column:   uint32(errNode.StartPoint().Column),
			Message:  "syntax error in AST",
		}
	}

	return &ValidationError{
		FilePath: filePath,
		Line:     0,
		Column:   0,
		Message:  "AST contains errors",
	}
}

// ASTErrors returns all ERROR node locations in the content for diagnostic reporting.
// Returns nil if no errors or unknown language.
func ASTErrors(content []byte, filePath string) []ValidationError {
	lang := languageForPath(filePath)
	if lang == nil {
		return nil
	}

	parser := sitter.NewParser()
	parser.SetLanguage(lang)

	tree, err := parser.ParseCtx(context.Background(), nil, content)
	if err != nil {
		return nil
	}

	root := tree.RootNode()
	if root == nil || !root.HasError() {
		return nil
	}

	var errs []ValidationError
	collectErrors(root, filePath, &errs)
	return errs
}

// findFirstError does a depth-first search for the first ERROR node.
func findFirstError(node *sitter.Node) *sitter.Node {
	if node.IsError() || node.IsMissing() {
		return node
	}
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.HasError() || child.IsError() || child.IsMissing() {
			found := findFirstError(child)
			if found != nil {
				return found
			}
		}
	}
	return nil
}

// collectErrors gathers all ERROR/MISSING nodes in the tree.
func collectErrors(node *sitter.Node, filePath string, errs *[]ValidationError) {
	if node.IsError() || node.IsMissing() {
		*errs = append(*errs, ValidationError{
			FilePath: filePath,
			Line:     uint32(node.StartPoint().Row),
			Column:   uint32(node.StartPoint().Column),
			Message:  "syntax error in AST",
		})
		return // don't recurse into error children
	}
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.HasError() || child.IsError() || child.IsMissing() {
			collectErrors(child, filePath, errs)
		}
	}
}

// languageForPath maps file extensions to tree-sitter languages.
// Mirrors the mapping in internal/ingest/engine.go.
func languageForPath(filePath string) *sitter.Language {
	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".go":
		return golang.GetLanguage()
	case ".py":
		return python.GetLanguage()
	case ".js":
		return javascript.GetLanguage()
	case ".ts", ".tsx":
		return typescript.GetLanguage()
	case ".sql":
		return sqllang.GetLanguage()
	default:
		return nil
	}
}
