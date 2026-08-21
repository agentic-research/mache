// Package linter runs warning-only static-analysis rules on write-back
// content. Since mache-73b885 the rules are SQL over the leyline daemon's
// emit_ast payload (SQL-shaped `_ast` rows from the SAME parse that
// validated the write) — no in-process tree-sitter.
package linter

import (
	"database/sql"
	"fmt"
	"strings"

	_ "modernc.org/sqlite" // pure-Go SQLite driver for the in-memory rule DB

	"github.com/agentic-research/mache/internal/leyline"
)

// Diagnostic is one warning-only lint finding. Line is 0-indexed.
type Diagnostic struct {
	Message string
	Line    uint32
}

func (d Diagnostic) String() string {
	return fmt.Sprintf("line %d: %s", d.Line+1, d.Message)
}

const nilSliceMessage = "Nil slice declaration. Consider 'make([]T, 0)' for JSON compatibility."

// nilSliceSQL flags `var x []T` declarations with no initializer.
//
// Structure over `_ast` rows: a `var_spec` node whose DIRECT child is a
// `slice_type` (the declared type) and which has NO direct `expression_list`
// child (the initializer). That excludes e.g. the slice_type inside
// `var w []int = []int{1}`'s composite literal, which sits under
// expression_list/composite_literal — two levels down, not one.
//
// Direct-child is read from an explicit parent_id column, NOT from arithmetic
// on the node id. This used to be three `substr`/`instr` expressions relying on
// the daemon's hierarchical path encoding (child id = parent id + "/" +
// segment). That encoding is under active review upstream
// (ley-line-open-17c271 proposes integer node keys), and depth-1 containment is
// not derivable from byte spans alone — so the assumption now lives in exactly
// one Go line where it is obvious, instead of being spread through SQL that
// would silently stop matching.
const nilSliceSQL = `
SELECT v.start_row
FROM _ast v
WHERE v.node_kind = 'var_spec'
  AND EXISTS (
    SELECT 1 FROM _ast c
    WHERE c.node_kind = 'slice_type' AND c.parent_id = v.node_id
  )
  AND NOT EXISTS (
    SELECT 1 FROM _ast c
    WHERE c.node_kind = 'expression_list' AND c.parent_id = v.node_id
  )
ORDER BY v.start_row`

// Lint checks the content for static analysis issues. Currently supports Go
// only; other languages return (nil, nil). It costs one leyline validate
// (emit_ast) round trip — callers that already hold an ASTPayload from the
// write path's validation should call LintAST instead to reuse that parse.
func Lint(content []byte, langName string) ([]Diagnostic, error) {
	if langName != "go" && !strings.HasSuffix(langName, ".go") {
		return nil, nil
	}
	res, err := leyline.ValidateContent(content, "go", "", true)
	if err != nil {
		return nil, err
	}
	return LintAST(res.AST)
}

// parentIDOf derives a node's parent from the daemon's hierarchical id
// encoding: a child's id is its parent's id plus "/" plus one segment. A
// top-level node (no separator) has no parent and yields "".
//
// THIS IS THE ONLY PLACE the linter assumes anything about node-id shape. It
// is isolated deliberately: ley-line-open-17c271 proposes replacing the path
// with an integer key, and emit_ast's ASTRow carries no parent field, so this
// derivation is what would have to change. Byte spans cannot substitute —
// they express descendant, not direct child, and a node sharing its parent's
// span (a parent with a single child) is indistinguishable by span alone.
func parentIDOf(nodeID string) string {
	i := strings.LastIndexByte(nodeID, '/')
	if i <= 0 {
		return ""
	}
	return nodeID[:i]
}

// LintAST runs the SQL rules over an already-parsed emit_ast payload — no
// daemon round trip. A nil payload (pass-through language, or a caller that
// validated without emit_ast) yields no diagnostics. Non-Go payloads yield no
// diagnostics: every current rule is Go-specific.
func LintAST(ast *leyline.ASTPayload) ([]Diagnostic, error) {
	if ast == nil || ast.Language != "go" || len(ast.AST) == 0 {
		return nil, nil
	}

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, fmt.Errorf("open in-memory lint db: %w", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec(`CREATE TABLE _ast (node_id TEXT PRIMARY KEY, parent_id TEXT, node_kind TEXT NOT NULL, start_row INTEGER NOT NULL)`); err != nil {
		return nil, fmt.Errorf("create lint _ast table: %w", err)
	}

	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin lint insert: %w", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO _ast (node_id, parent_id, node_kind, start_row) VALUES (?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("prepare lint insert: %w", err)
	}
	for _, row := range ast.AST {
		if _, err := stmt.Exec(row.NodeID, parentIDOf(row.NodeID), row.NodeKind, row.StartRow); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			return nil, fmt.Errorf("insert lint _ast row %s: %w", row.NodeID, err)
		}
	}
	_ = stmt.Close()
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit lint insert: %w", err)
	}

	rows, err := db.Query(nilSliceSQL)
	if err != nil {
		return nil, fmt.Errorf("run nil-slice rule: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var diags []Diagnostic
	for rows.Next() {
		var startRow uint32
		if err := rows.Scan(&startRow); err != nil {
			return nil, fmt.Errorf("scan nil-slice row: %w", err)
		}
		diags = append(diags, Diagnostic{Message: nilSliceMessage, Line: startRow})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate nil-slice rows: %w", err)
	}
	return diags, nil
}
