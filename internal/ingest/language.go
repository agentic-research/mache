package ingest

import (
	sitter "github.com/smacker/go-tree-sitter"

	"github.com/agentic-research/mache/internal/lang"
)

// DetectLanguageFromExt returns the language name and tree-sitter Language
// for a given file extension. Returns ok=false for unsupported extensions.
// Thin wrapper over lang.ForExt for backward compatibility.
func DetectLanguageFromExt(ext string) (langName string, grammar *sitter.Language, ok bool) {
	l := lang.ForExt(ext)
	if l == nil {
		return "", nil, false
	}
	return l.Name, l.Grammar(), true
}
