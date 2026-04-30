package cmd

import (
	"database/sql"
	"log"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/agentic-research/mache/internal/ingest"
)

// pickCallExtractor returns the pure-Go ASTWalker-backed extractor
// when `_ast` is present in the active .db, falling back to the
// CGO SitterWalker-backed extractor otherwise. This is the per-site
// dispatch point used by mache serve / mache mount when wiring a
// CallExtractor onto a SQLiteGraph.
//
// Detection is a single SQL query against sqlite_master. Errors
// during detection log + fall through to the CGO extractor — never
// take down the wiring on a transient SQL hiccup.
//
// ADR-0012 step 3 makes this picker unconditional (always AST) once
// `mache build` invokes `leyline parse`. Step 4 deletes the CGO
// branch entirely along with newCallExtractor and SitterWalker.
func pickCallExtractor(db *sql.DB) graph.CallExtractor {
	if db == nil {
		return newCallExtractor()
	}
	var hasAST int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='_ast'`,
	).Scan(&hasAST)
	if err != nil {
		log.Printf("call extractor: _ast detection failed (%v); using CGO fallback", err)
		return newCallExtractor()
	}
	if hasAST == 0 {
		return newCallExtractor()
	}
	return newASTCallExtractor(db)
}

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
