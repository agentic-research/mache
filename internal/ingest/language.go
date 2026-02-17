package ingest

import (
	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/hcl"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/rust"
	"github.com/smacker/go-tree-sitter/sql"
	"github.com/smacker/go-tree-sitter/typescript/typescript"
	"github.com/smacker/go-tree-sitter/yaml"
)


// DetectLanguageFromExt returns the language name and tree-sitter Language
// for a given file extension. Returns ok=false for unsupported extensions.
func DetectLanguageFromExt(ext string) (langName string, lang *sitter.Language, ok bool) {
	switch ext {
	case ".go":
		return "go", golang.GetLanguage(), true
	case ".py":
		return "python", python.GetLanguage(), true
	case ".tf", ".hcl":
		return "terraform", hcl.GetLanguage(), true
	case ".js":
		return "javascript", javascript.GetLanguage(), true
	case ".ts", ".tsx":
		return "typescript", typescript.GetLanguage(), true
	case ".rs":
		return "rust", rust.GetLanguage(), true
	case ".sql":
		return "sql", sql.GetLanguage(), true
	case ".yaml", ".yml":
		return "yaml", yaml.GetLanguage(), true
	default:
		return "", nil, false
	}
}

// LanguageProfile defines how to extract semantic structure from AST nodes
// for languages that don't use field names (like HCL)
type LanguageProfile struct {
	// EnrichNode adds synthetic attributes to records for languages without field names
	EnrichNode func(n *sitter.Node, rec map[string]any)
}

// GetLanguageProfile returns a profile for the given language name
func GetLanguageProfile(langName string) *LanguageProfile {
	switch langName {
	case "hcl", "terraform":
		return &LanguageProfile{
			EnrichNode: enrichHCLNode,
		}
	default:
		return nil
	}
}

// enrichHCLNode adds synthetic attributes for HCL blocks
// HCL blocks have structure: identifier string_lit* block_start body block_end
// We need to infer "has_name" and "has_body" from child types
func enrichHCLNode(n *sitter.Node, rec map[string]any) {
	if n.Type() != "block" {
		return
	}

	// Scan children to find structure
	var (
		hasIdentifier  bool
		stringLitCount int
		hasBody        bool
	)

	count := int(n.ChildCount())
	for i := 0; i < count; i++ {
		child := n.Child(i)
		if child == nil || !child.IsNamed() {
			continue
		}

		switch child.Type() {
		case "identifier":
			hasIdentifier = true
		case "string_lit":
			stringLitCount++
		case "body":
			hasBody = true
		}
	}

	// CRITICAL: Change the type based on structure
	// This ensures FCA only groups blocks with consistent structure
	if hasIdentifier && stringLitCount > 0 && hasBody {
		// This is a named container block - change type so FCA treats it distinctly
		rec["type"] = "hcl_container"
		rec["has_name"] = true
		rec["field_name_type"] = "string_lit"
		rec["has_body"] = true
		rec["field_body_type"] = "body"
	}
	// Blocks without name+body keep type="block" and won't be used as containers
}
