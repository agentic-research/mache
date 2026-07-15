//go:build unix

package writeback

// Gated end-to-end tests against the REAL pinned leyline daemon
// (~/.mache/bin/leyline, version-checked against the binary pin; skipped when
// absent, never downloaded). These verify the actual tree-sitter verdicts and
// positions the daemon produces — the unit suite only pins the client-side
// wire contract.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/lltest"
)

func usePinnedDaemon(t *testing.T) {
	t.Helper()
	sock := lltest.StartPinnedDaemon(t)
	t.Setenv("LEYLINE_SOCKET", sock)
}

func TestE2E_Validate_ValidGo(t *testing.T) {
	usePinnedDaemon(t)
	assert.NoError(t, Validate([]byte("package main\n\nfunc hello() string {\n\treturn \"world\"\n}\n"), "test.go"))
}

func TestE2E_Validate_BrokenGo_Positions(t *testing.T) {
	usePinnedDaemon(t)

	// Verified against the v0.7.8 daemon: the first ERROR node for this
	// buffer is at 0-based row 2, col 12 (the `{ BROKEN SYNTAX` region).
	err := Validate([]byte("package main\n\nfunc main() { BROKEN SYNTAX "), "test.go")
	require.Error(t, err)

	var ve *ValidationError
	require.ErrorAs(t, err, &ve)
	assert.Equal(t, "test.go", ve.FilePath)
	assert.Equal(t, uint32(2), ve.Line)
	assert.Equal(t, uint32(12), ve.Column)
	assert.Contains(t, ve.Message, "syntax error")
}

func TestE2E_Validate_BrokenGo_MissingBrace(t *testing.T) {
	usePinnedDaemon(t)
	err := Validate([]byte("package main\n\nfunc hello() string {\n\treturn \"world\"\n// missing closing brace\n"), "test.go")
	require.Error(t, err)
	var ve *ValidationError
	require.ErrorAs(t, err, &ve)
}

func TestE2E_Validate_BrokenPython(t *testing.T) {
	usePinnedDaemon(t)
	err := Validate([]byte("def hello(\n    return \"world\"\n"), "test.py")
	require.Error(t, err)
	var ve *ValidationError
	require.ErrorAs(t, err, &ve)
}

func TestE2E_Validate_ValidPython(t *testing.T) {
	usePinnedDaemon(t)
	assert.NoError(t, Validate([]byte("def hello():\n    return \"world\"\n"), "test.py"))
}

func TestE2E_Validate_TerraformPassThrough(t *testing.T) {
	// Preserved-and-documented behavior: .tf is not in the daemon's
	// validate set, so even syntactically broken HCL passes validation
	// (FormatBuffer's hclwrite is the remaining structural check).
	usePinnedDaemon(t)
	assert.NoError(t, Validate([]byte(`resource "x" { broken {{{`), "main.tf"))
}

func TestE2E_ValidateWithAST_ReturnsRowsFromSameParse(t *testing.T) {
	usePinnedDaemon(t)

	src := []byte("package main\n\nvar x []int\n\nfunc f() {\n\tvar y []string = nil\n\t_ = y\n}\n")
	ast, err := ValidateWithAST(src, "main.go")
	require.NoError(t, err)
	require.NotNil(t, ast, "clean Go parse with emit must return the AST payload")
	assert.Equal(t, "go", ast.Language)
	assert.Equal(t, "main.go", ast.SourceID, "path must ride as source_id on emitted rows")
	assert.NotEmpty(t, ast.ContentHash)

	kinds := map[string]int{}
	for _, r := range ast.AST {
		kinds[r.NodeKind]++
		assert.Equal(t, "main.go", r.SourceID)
	}
	assert.GreaterOrEqual(t, kinds["var_spec"], 2, "both var declarations must appear as _ast rows")
	assert.GreaterOrEqual(t, kinds["function_declaration"], 1)
	require.NotEmpty(t, ast.Defs, "func f must be extracted as a def")
	assert.Equal(t, "f", ast.Defs[0].Token)
}

func TestE2E_ASTErrors_EnumeratesAll(t *testing.T) {
	usePinnedDaemon(t)
	errs := ASTErrors([]byte("package main\n\nfunc hello() {\n\tx :=\n}\n"), "test.go")
	require.NotEmpty(t, errs)
	assert.Equal(t, "test.go", errs[0].FilePath)
}
