package cmd

import (
	"context"
	"sync"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/lang"
	sitter "github.com/smacker/go-tree-sitter"
)

// newCallExtractor creates a CallExtractor that uses a sync.Pool of tree-sitter
// parsers to reduce allocation overhead. Safe for concurrent use.
func newCallExtractor() graph.CallExtractor {
	walker := ingest.NewSitterWalker()
	pool := &sync.Pool{
		New: func() any { return sitter.NewParser() },
	}
	return func(content []byte, path, langName string) ([]graph.QualifiedCall, error) {
		l := lang.ForName(langName)
		if l == nil {
			return nil, nil
		}
		grammar := l.Grammar()
		parser := pool.Get().(*sitter.Parser)
		defer pool.Put(parser)
		parser.SetLanguage(grammar)
		tree, _ := parser.ParseCtx(context.Background(), nil, content)
		if tree == nil { // coverage:ignore — tree-sitter ParseCtx with a valid grammar returns a non-nil tree (empty content yields a tree with an empty root); defensive nil-guard for future API drift
			return nil, nil // coverage:ignore
		} // coverage:ignore
		return walker.ExtractQualifiedCalls(tree.RootNode(), content, grammar, langName)
	}
}
