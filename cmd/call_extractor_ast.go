package cmd

import (
	"database/sql"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/agentic-research/mache/internal/ingest"
)

// newASTCallExtractor returns a graph.CallExtractor backed by SQL queries
// against the `_ast` table — no CGO, no tree-sitter parser. The DB must
// have been parsed by ley-line (or any source that populates `_ast`
// with the schema mache expects).
//
// This is the pure-Go alternative to newCallExtractor (mount.go) for
// backends that already carry parsed AST. ADR-0012 (mache-37ae8b)
// migration plan calls for this to replace newCallExtractor entirely
// once mache build delegates parsing to ley-line. Until then, both
// coexist and callers select per-backend.
//
// The extractor's signature matches graph.CallExtractor:
//
//	func(content []byte, path, langName string) ([]graph.QualifiedCall, error)
//
// `content` is unused — ASTWalker resolves the call set from the
// pre-parsed AST keyed on `path` (used as source_id). `langName`
// selects the per-language CallPattern registry.
//
// Returns nil, nil when:
//   - langName has no registered call pattern (same shape as the
//     SitterWalker-backed extractor's "no grammar" branch)
//   - the source row isn't in `_ast` (a stale path argument)
//
// Errors are returned only for SQL failures the caller should propagate.
func newASTCallExtractor(db *sql.DB) graph.CallExtractor {
	walker := ingest.NewASTWalker(db)
	return func(_ []byte, path, langName string) ([]graph.QualifiedCall, error) {
		return walker.ExtractQualifiedCalls(path, langName)
	}
}
