//go:build unix

package linter

// Gated e2e: the full Lint path (one leyline validate emit_ast round trip +
// SQL rules) against the REAL pinned daemon. Skips when the pinned binary is
// absent; never downloads.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/lltest"
)

func TestE2E_Lint_NilSlice_Flagged(t *testing.T) {
	lltest.UsePinnedDaemon(t)

	src := []byte("package main\n\nvar x []int\n\nfunc f() {\n\tvar z []byte\n\t_ = z\n}\n")
	diags, err := Lint(src, "go")
	require.NoError(t, err)
	require.Len(t, diags, 2, "both uninitialized slice declarations must be flagged")
	assert.Equal(t, uint32(2), diags[0].Line, "top-level var x []int (0-based row 2)")
	assert.Equal(t, uint32(5), diags[1].Line, "in-function var z []byte (0-based row 5)")
	assert.Contains(t, diags[0].Message, "Nil slice declaration")
}

func TestE2E_Lint_InitializedSlices_Clean(t *testing.T) {
	lltest.UsePinnedDaemon(t)

	src := []byte("package main\n\nvar y []string = nil\n\nfunc f() {\n\tw := []int{1}\n\t_ = w\n\t_ = y\n}\n")
	diags, err := Lint(src, "go")
	require.NoError(t, err)
	assert.Empty(t, diags)
}

func TestE2E_Lint_FunctionSnippet_Parses(t *testing.T) {
	// NFS write-back sends function-body snippets without a package clause;
	// tree-sitter-go accepts them, so lint must work on snippets too.
	lltest.UsePinnedDaemon(t)

	src := []byte("func HelloWorld() {\n\tvar s []int\n\t_ = s\n}\n")
	diags, err := Lint(src, "go")
	require.NoError(t, err)
	require.Len(t, diags, 1)
	assert.Equal(t, uint32(1), diags[0].Line)
}
