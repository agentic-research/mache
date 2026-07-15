package leyline_test

// External test package (leyline_test) so these tests can use
// lltest.FakeDaemon, which imports internal/leyline — an in-package test file
// would create an import cycle. What is under test is the package-level
// ValidateContent path: daemon acquisition via LEYLINE_SOCKET, one dial, one
// op, typed decode.

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/leyline"
	"github.com/agentic-research/mache/internal/lltest"
)

func TestValidateContent_DecodesVerdictAndAST(t *testing.T) {
	var gotReq map[string]any
	sock := lltest.FakeDaemon(t, func(req map[string]any) any {
		gotReq = req
		return map[string]any{
			"ok":          true,
			"errors":      []any{},
			"diagnostics": []any{},
			"ast": map[string]any{
				"source_id":    "main.go",
				"language":     "go",
				"content_hash": "cafe",
				"ast": []any{map[string]any{
					"node_id": "main.go", "source_id": "main.go", "node_kind": "source_file",
					"start_byte": 0, "end_byte": 13, "start_row": 0, "start_col": 0,
					"end_row": 1, "end_col": 0, "node_hash": "beef",
				}},
				"defs":    []any{map[string]any{"token": "f", "node_id": "main.go/function_declaration", "source_id": "main.go", "container_node_id": nil, "canonical_kind": "function"}},
				"refs":    []any{},
				"imports": []any{},
			},
		}
	})
	t.Setenv("LEYLINE_SOCKET", sock)

	res, err := leyline.ValidateContent([]byte("package main\n"), "go", "main.go", true)
	require.NoError(t, err)
	assert.True(t, res.OK)
	assert.Empty(t, res.Errors)
	require.NotNil(t, res.AST)
	assert.Equal(t, "go", res.AST.Language)
	require.Len(t, res.AST.AST, 1)
	assert.Equal(t, "source_file", res.AST.AST[0].NodeKind)
	require.Len(t, res.AST.Defs, 1)
	assert.Equal(t, "f", res.AST.Defs[0].Token)
	require.NotNil(t, res.AST.Defs[0].CanonicalKind)
	assert.Equal(t, "function", *res.AST.Defs[0].CanonicalKind)

	require.NotNil(t, gotReq)
	assert.Equal(t, "validate", gotReq["op"])
	assert.Equal(t, "go", gotReq["language"])
	assert.Equal(t, "main.go", gotReq["path"])
	assert.Equal(t, true, gotReq["emit_ast"])
}

func TestValidateContent_SyntaxErrorsDecoded(t *testing.T) {
	sock := lltest.FakeDaemon(t, func(map[string]any) any {
		return map[string]any{
			"ok": false,
			"errors": []any{
				map[string]any{"row": 2, "col": 12, "byte_start": 26, "byte_end": 41, "message": "syntax error"},
			},
			"diagnostics": []any{},
		}
	})
	t.Setenv("LEYLINE_SOCKET", sock)

	res, err := leyline.ValidateContent([]byte("package main\nbroken"), "go", "", false)
	require.NoError(t, err, "a not-ok verdict is a RESULT, not a client error")
	assert.False(t, res.OK)
	require.Len(t, res.Errors, 1)
	assert.Equal(t, uint32(2), res.Errors[0].Row)
	assert.Equal(t, uint32(12), res.Errors[0].Col)
	assert.Equal(t, uint32(26), res.Errors[0].ByteStart)
	assert.Equal(t, uint32(41), res.Errors[0].ByteEnd)
}

func TestValidateContent_MissingErrorsKey_DaemonTooOld(t *testing.T) {
	sock := lltest.FakeDaemon(t, func(map[string]any) any {
		return map[string]any{"ok": true, "diagnostics": []any{}}
	})
	t.Setenv("LEYLINE_SOCKET", sock)

	_, err := leyline.ValidateContent([]byte("package main\n"), "go", "", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), leyline.PinnedBinaryVersion())
}

func TestValidateContent_ErrorEnvelope(t *testing.T) {
	sock := lltest.FakeDaemon(t, func(map[string]any) any {
		return map[string]any{"ok": false, "error": "unknown language id: `xyz`"}
	})
	t.Setenv("LEYLINE_SOCKET", sock)

	_, err := leyline.ValidateContent([]byte("x"), "xyz", "", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown language id")
}

func TestPinnedBinaryVersion_IsSemverTag(t *testing.T) {
	assert.Regexp(t, regexp.MustCompile(`^v\d+\.\d+\.\d+$`), leyline.PinnedBinaryVersion(),
		"pin must be an exact semver tag — test gates compare local binaries against it")
}
