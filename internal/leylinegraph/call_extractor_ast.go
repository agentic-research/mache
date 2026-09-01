package leylinegraph

import (
	"database/sql"
	"log"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/ingest"
)

// PickCallExtractor returns the pure-Go ASTWalker-backed extractor when `_ast`
// is present in the active .db, and a no-op extractor otherwise. This is the
// per-site dispatch point used by mache serve / mache mount when wiring a
// CallExtractor onto a SQLiteGraph.
//
// Detection is a single SQL query against sqlite_master. ADR-0012 step 4
// removed in-process CGO tree-sitter, so there is no CGO fallback: a db with
// no `_ast` table (or a nil db, or a detection error) yields the no-op
// extractor rather than crashing the wiring. Every source projection carries
// `_ast` (ley-line parses it), so the no-op only applies to non-source backends.
func PickCallExtractor(db *sql.DB) graph.CallExtractor {
	if db == nil {
		return NoopCallExtractor()
	}
	var hasAST int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='_ast'`,
	).Scan(&hasAST)
	if err != nil {
		log.Printf("call extractor: _ast detection failed (%v); using no-op extractor", err)
		return NoopCallExtractor()
	}
	if hasAST == 0 {
		return NoopCallExtractor()
	}
	return NewASTCallExtractor(db)
}

// PickScopedCallExtractor mirrors PickCallExtractor but returns a
// graph.ScopedCallExtractor (nil when `_ast` isn't present). This is the
// extractor GetCallees actually uses once a construct carries
// ast_source_id/ast_scope_id Properties (bead mache-fd9982) — the legacy
// graph.CallExtractor returned by PickCallExtractor is kept only as a
// fallback for constructs/.dbs that predate this fix.
func PickScopedCallExtractor(db *sql.DB) graph.ScopedCallExtractor {
	if db == nil {
		return nil
	}
	var hasAST int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='_ast'`,
	).Scan(&hasAST)
	if err != nil || hasAST == 0 {
		return nil
	}
	return NewASTScopedCallExtractor(db)
}

// NoopCallExtractor returns a CallExtractor that resolves no calls. Used for
// backends with no pre-parsed `_ast` table (e.g. JSON/data-only graphs, or the
// composite cross-mount fallback when a mount carries no AST). Since ADR-0012
// step 4 removed in-process CGO tree-sitter, there is no parser to fall back
// to — the honest answer for a non-source backend is "no calls".
func NoopCallExtractor() graph.CallExtractor {
	return func(_ []byte, _, _ string) ([]graph.QualifiedCall, error) {
		return nil, nil
	}
}

// NewASTCallExtractor returns a graph.CallExtractor backed by SQL queries
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
func NewASTCallExtractor(db *sql.DB) graph.CallExtractor {
	walker := ingest.NewASTWalker(db)
	return func(_ []byte, path, langName string) ([]graph.QualifiedCall, error) {
		return walker.ExtractQualifiedCalls(path, langName)
	}
}

// NewASTScopedCallExtractor returns a graph.ScopedCallExtractor backed by SQL
// queries against the `_ast` table, scoped to a single construct. Unlike
// NewASTCallExtractor, sourceID and scopeID here are the REAL `_ast`/`_source`
// key and scope node id (recovered from the construct's ast_source_id/
// ast_scope_id Properties) — not a graph node id — so the query actually
// matches rows. This is the fix for bead mache-fd9982: find_callees was
// silently broken on the serve/mount path because the old wiring fed a graph
// node id (e.g. "cmd/functions/evalOrAbs") into a query keyed on real _ast
// source ids (e.g. "agent.go"), which matched nothing.
func NewASTScopedCallExtractor(db *sql.DB) graph.ScopedCallExtractor {
	walker := ingest.NewASTWalker(db)
	return func(sourceID, scopeID, langName string) ([]graph.QualifiedCall, error) {
		return walker.ExtractQualifiedCallsScoped(sourceID, scopeID, langName)
	}
}
