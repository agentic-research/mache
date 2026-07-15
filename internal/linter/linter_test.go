package linter

// Unit tests for the SQL-over-emit_ast lint rules. LintAST is pure (no
// daemon): payloads here are hand-built to mirror the exact node_id/node_kind
// shapes the v0.7.8 daemon emits (verified against a live daemon — see the
// e2e file for the real-parse path).

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/leyline"
)

// row is a shorthand _ast row constructor for rule tests (only the columns
// the rules consult are populated).
func row(nodeID, kind string, startRow uint32) leyline.ASTRow {
	return leyline.ASTRow{NodeID: nodeID, SourceID: "main.go", NodeKind: kind, StartRow: startRow}
}

// goPayload wraps rows in a Go-language payload.
func goPayload(rows ...leyline.ASTRow) *leyline.ASTPayload {
	return &leyline.ASTPayload{SourceID: "main.go", Language: "go", AST: rows}
}

func TestLintAST_NilSliceDeclaration_Flagged(t *testing.T) {
	// var x []int   → var_spec with a direct slice_type child, no value
	ast := goPayload(
		row("main.go", "source_file", 0),
		row("main.go/var_declaration", "var_declaration", 2),
		row("main.go/var_declaration/var_spec", "var_spec", 2),
		row("main.go/var_declaration/var_spec/identifier", "identifier", 2),
		row("main.go/var_declaration/var_spec/slice_type", "slice_type", 2),
		row("main.go/var_declaration/var_spec/slice_type/type_identifier", "type_identifier", 2),
	)
	diags, err := LintAST(ast)
	require.NoError(t, err)
	require.Len(t, diags, 1)
	assert.Equal(t, uint32(2), diags[0].Line)
	assert.Contains(t, diags[0].Message, "Nil slice declaration")
	assert.Contains(t, diags[0].String(), "line 3:", "String() renders 1-based")
}

func TestLintAST_InitializedSlice_NotFlagged(t *testing.T) {
	// var y []string = nil → var_spec has a direct expression_list child
	ast := goPayload(
		row("main.go", "source_file", 0),
		row("main.go/var_declaration", "var_declaration", 4),
		row("main.go/var_declaration/var_spec", "var_spec", 4),
		row("main.go/var_declaration/var_spec/identifier", "identifier", 4),
		row("main.go/var_declaration/var_spec/slice_type", "slice_type", 4),
		row("main.go/var_declaration/var_spec/slice_type/type_identifier", "type_identifier", 4),
		row("main.go/var_declaration/var_spec/expression_list", "expression_list", 4),
		row("main.go/var_declaration/var_spec/expression_list/nil", "nil", 4),
	)
	diags, err := LintAST(ast)
	require.NoError(t, err)
	assert.Empty(t, diags)
}

func TestLintAST_CompositeLiteralSliceType_NotDirectChild_NotFlagged(t *testing.T) {
	// var w = []int{1} → the slice_type sits under expression_list/
	// composite_literal, NOT as a direct var_spec child. The direct-child
	// predicate (one path segment) must exclude it.
	ast := goPayload(
		row("main.go", "source_file", 0),
		row("main.go/var_declaration", "var_declaration", 1),
		row("main.go/var_declaration/var_spec", "var_spec", 1),
		row("main.go/var_declaration/var_spec/identifier", "identifier", 1),
		row("main.go/var_declaration/var_spec/expression_list", "expression_list", 1),
		row("main.go/var_declaration/var_spec/expression_list/composite_literal", "composite_literal", 1),
		row("main.go/var_declaration/var_spec/expression_list/composite_literal/slice_type", "slice_type", 1),
	)
	diags, err := LintAST(ast)
	require.NoError(t, err)
	assert.Empty(t, diags)
}

func TestLintAST_MapOfSlices_NotFlagged(t *testing.T) {
	// var m map[string][]int → declared type is map_type; the slice_type is
	// a grandchild and must not trigger the rule.
	ast := goPayload(
		row("main.go", "source_file", 0),
		row("main.go/var_declaration", "var_declaration", 1),
		row("main.go/var_declaration/var_spec", "var_spec", 1),
		row("main.go/var_declaration/var_spec/identifier", "identifier", 1),
		row("main.go/var_declaration/var_spec/map_type", "map_type", 1),
		row("main.go/var_declaration/var_spec/map_type/type_identifier", "type_identifier", 1),
		row("main.go/var_declaration/var_spec/map_type/slice_type", "slice_type", 1),
	)
	diags, err := LintAST(ast)
	require.NoError(t, err)
	assert.Empty(t, diags)
}

func TestLintAST_MultipleNilSlices_AllFlaggedInOrder(t *testing.T) {
	// Same-kind siblings carry _N suffixes on node_id (daemon behavior for
	// >1 named children of one kind) — the rule must still see them.
	ast := goPayload(
		row("main.go", "source_file", 0),
		row("main.go/function_declaration", "function_declaration", 0),
		row("main.go/function_declaration/block", "block", 0),
		row("main.go/function_declaration/block/statement_list", "statement_list", 1),
		row("main.go/function_declaration/block/statement_list/var_declaration_0", "var_declaration", 1),
		row("main.go/function_declaration/block/statement_list/var_declaration_0/var_spec", "var_spec", 1),
		row("main.go/function_declaration/block/statement_list/var_declaration_0/var_spec/slice_type", "slice_type", 1),
		row("main.go/function_declaration/block/statement_list/var_declaration_1", "var_declaration", 3),
		row("main.go/function_declaration/block/statement_list/var_declaration_1/var_spec", "var_spec", 3),
		row("main.go/function_declaration/block/statement_list/var_declaration_1/var_spec/slice_type", "slice_type", 3),
	)
	diags, err := LintAST(ast)
	require.NoError(t, err)
	require.Len(t, diags, 2)
	assert.Equal(t, uint32(1), diags[0].Line)
	assert.Equal(t, uint32(3), diags[1].Line)
}

func TestLintAST_NilPayload_NoDiagnostics(t *testing.T) {
	diags, err := LintAST(nil)
	require.NoError(t, err)
	assert.Nil(t, diags)
}

func TestLintAST_NonGoPayload_NoDiagnostics(t *testing.T) {
	ast := &leyline.ASTPayload{SourceID: "lib.rs", Language: "rust", AST: []leyline.ASTRow{
		row("lib.rs", "source_file", 0),
	}}
	diags, err := LintAST(ast)
	require.NoError(t, err)
	assert.Nil(t, diags)
}

func TestLint_NonGoLanguage_NoDaemonContact(t *testing.T) {
	// Lint's language gate must short-circuit before any daemon work.
	t.Setenv("LEYLINE_SOCKET", "/nonexistent/sock")
	diags, err := Lint([]byte("resource {}"), "terraform")
	require.NoError(t, err)
	assert.Nil(t, diags)
}
