package writeback

// Unit tests for the leyline-backed validator. These run WITHOUT a real
// leyline: lltest.FakeDaemon serves canned wire responses over a private UDS
// socket that LEYLINE_SOCKET points at, so what is under test is the
// client-side contract — request shape, position mapping (0-based wire →
// 0-based ValidationError), pass-through gating, and the hard daemon-too-old
// error. Real-parser behavior is covered by validate_e2e_test.go against the
// pinned binary.

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/lltest"
)

// okValidateResp is a clean-parse validate response per the v0.7.8 wire.
func okValidateResp() map[string]any {
	return map[string]any{"ok": true, "errors": []any{}, "diagnostics": []any{}}
}

// useFakeDaemon points the leyline discovery path at a fake daemon.
func useFakeDaemon(t *testing.T, handler lltest.Handler) {
	t.Helper()
	sock := lltest.FakeDaemon(t, handler)
	t.Setenv("LEYLINE_SOCKET", sock)
}

// useDeadSocket points LEYLINE_SOCKET at a plain file so any attempt to
// contact the daemon fails fast (and provably: a pass-through result can
// only mean the daemon was never contacted).
func useDeadSocket(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	dead := filepath.Join(dir, "dead.sock")
	require.NoError(t, os.WriteFile(dead, nil, 0o600))
	t.Setenv("LEYLINE_SOCKET", dead)
}

func TestValidate_ValidGo_FakeDaemon(t *testing.T) {
	var gotReq map[string]any
	useFakeDaemon(t, func(req map[string]any) any {
		gotReq = req
		return okValidateResp()
	})

	err := Validate([]byte("package main\n"), "test.go")
	assert.NoError(t, err)

	require.NotNil(t, gotReq, "daemon must be consulted for .go content")
	assert.Equal(t, "validate", gotReq["op"])
	assert.Equal(t, "go", gotReq["language"], "language key must be sent (it wins over path)")
	assert.Equal(t, "test.go", gotReq["path"])
	assert.Equal(t, "package main\n", gotReq["content"])
	_, hasEmit := gotReq["emit_ast"]
	assert.False(t, hasEmit, "plain Validate must not request emit_ast")
}

func TestValidate_BrokenGo_MapsFirstErrorPosition(t *testing.T) {
	useFakeDaemon(t, func(map[string]any) any {
		return map[string]any{
			"ok": false,
			"errors": []any{
				map[string]any{"row": 2, "col": 12, "byte_start": 26, "byte_end": 41, "message": "syntax error"},
				map[string]any{"row": 4, "col": 0, "byte_start": 50, "byte_end": 50, "message": "missing }"},
			},
			"diagnostics": []any{map[string]any{"line": 2, "col": 12, "message": "syntax error"}},
		}
	})

	err := Validate([]byte("package main\n\nfunc main() { BROKEN SYNTAX \n\n"), "test.go")
	require.Error(t, err)

	var ve *ValidationError
	require.ErrorAs(t, err, &ve)
	assert.Equal(t, "test.go", ve.FilePath)
	assert.Equal(t, uint32(2), ve.Line, "wire row is 0-based and ValidationError.Line stays 0-based")
	assert.Equal(t, uint32(12), ve.Column, "wire col is 0-based and ValidationError.Column stays 0-based")
	assert.Contains(t, ve.Message, "syntax error")
	// Error() renders 1-based for humans — the historical contract.
	assert.Contains(t, ve.Error(), "test.go:3:13:")
}

func TestValidate_DaemonWithoutErrorsKey_IsHardError(t *testing.T) {
	// A pre-v0.7.7 daemon (user-supplied via LEYLINE_SOCKET) has no
	// `errors` key. That must be a hard "daemon too old" failure, NOT a
	// silent pass — otherwise invalid bytes reach the splice.
	useFakeDaemon(t, func(map[string]any) any {
		return map[string]any{"ok": true, "diagnostics": []any{}}
	})

	err := Validate([]byte("package main\n"), "test.go")
	require.Error(t, err)
	var ve *ValidationError
	assert.False(t, errors.As(err, &ve), "daemon-too-old must not be a ValidationError")
	assert.Contains(t, err.Error(), "v0.7.8")
}

func TestValidate_DaemonErrorEnvelope_IsHardError(t *testing.T) {
	useFakeDaemon(t, func(map[string]any) any {
		return map[string]any{"ok": false, "error": "unknown op `validate`"}
	})

	err := Validate([]byte("package main\n"), "test.go")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown op")
}

func TestValidate_UnknownExtension_PassThroughWithoutDaemon(t *testing.T) {
	// .txt is never validated; the daemon must not even be contacted —
	// the dead socket would error if it were.
	useDeadSocket(t)
	assert.NoError(t, Validate([]byte(`this is not valid code in any language {{{`), "test.txt"))
}

func TestValidate_TerraformExtension_PassThroughWithoutDaemon(t *testing.T) {
	// Documented behavior change (mache-73b885): .tf/.hcl were validated by
	// the old in-process grammar set but are NOT in the leyline daemon's
	// validate language set — they now pass through unvalidated (hclwrite
	// in FormatBuffer remains the only structural check for them).
	useDeadSocket(t)
	assert.NoError(t, Validate([]byte(`resource "x" { broken {{{`), "main.tf"))
}

func TestValidate_DaemonUnreachable_IsErrorNotPass(t *testing.T) {
	// For a VALIDATED language, an unreachable daemon must surface as an
	// error (draft mode on the write path), never as a false "valid".
	useDeadSocket(t)
	err := Validate([]byte("package main\n"), "test.go")
	require.Error(t, err)
	var ve *ValidationError
	assert.False(t, errors.As(err, &ve), "acquisition failure must not masquerade as a syntax error")
}

func TestValidate_EmptyContent(t *testing.T) {
	useFakeDaemon(t, func(map[string]any) any { return okValidateResp() })
	assert.NoError(t, Validate([]byte{}, "test.go"))
}

func TestValidateWithAST_RequestsEmitForGo(t *testing.T) {
	var gotReq map[string]any
	useFakeDaemon(t, func(req map[string]any) any {
		gotReq = req
		resp := okValidateResp()
		resp["ast"] = map[string]any{
			"source_id":    "test.go",
			"language":     "go",
			"content_hash": "abc",
			"ast": []any{map[string]any{
				"node_id": "test.go", "source_id": "test.go", "node_kind": "source_file",
				"start_byte": 0, "end_byte": 13, "start_row": 0, "start_col": 0,
				"end_row": 1, "end_col": 0, "node_hash": "deadbeef",
			}},
			"defs": []any{}, "refs": []any{}, "imports": []any{},
		}
		return resp
	})

	ast, err := ValidateWithAST([]byte("package main\n"), "test.go")
	require.NoError(t, err)
	require.NotNil(t, gotReq)
	assert.Equal(t, true, gotReq["emit_ast"], "ValidateWithAST must fold emit_ast into the same request")
	require.NotNil(t, ast, "AST payload must round-trip")
	assert.Equal(t, "go", ast.Language)
	require.Len(t, ast.AST, 1)
	assert.Equal(t, "source_file", ast.AST[0].NodeKind)
	assert.Equal(t, uint32(13), ast.AST[0].EndByte)
}

func TestValidateWithAST_EmitRequestedButMissing_IsHardError(t *testing.T) {
	// A v0.7.7 daemon knows `errors` but silently ignores emit_ast. The
	// missing `ast` payload must be a hard error, not a silent lint skip.
	useFakeDaemon(t, func(map[string]any) any { return okValidateResp() })

	_, err := ValidateWithAST([]byte("package main\n"), "test.go")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "v0.7.8")
}

func TestValidateWithAST_NonGoValidatedLanguage_NoEmit(t *testing.T) {
	// Python is validated but has no lint rules — emit_ast must NOT be
	// requested (the daemon's extractor supports fewer languages than its
	// validator, and requesting emit for an uncovered one is a daemon-side
	// hard error).
	var gotReq map[string]any
	useFakeDaemon(t, func(req map[string]any) any {
		gotReq = req
		return okValidateResp()
	})

	ast, err := ValidateWithAST([]byte("def f():\n    pass\n"), "test.py")
	require.NoError(t, err)
	assert.Nil(t, ast)
	require.NotNil(t, gotReq)
	_, hasEmit := gotReq["emit_ast"]
	assert.False(t, hasEmit)
	assert.Equal(t, "py", gotReq["language"])
}

func TestValidateWithAST_PassThroughExtension_NilNil(t *testing.T) {
	useDeadSocket(t)
	ast, err := ValidateWithAST([]byte("whatever"), "notes.md")
	assert.NoError(t, err)
	assert.Nil(t, ast)
}

func TestASTErrors_MapsAllErrors(t *testing.T) {
	useFakeDaemon(t, func(map[string]any) any {
		return map[string]any{
			"ok": false,
			"errors": []any{
				map[string]any{"row": 1, "col": 4, "byte_start": 10, "byte_end": 12, "message": "syntax error"},
				map[string]any{"row": 3, "col": 0, "byte_start": 30, "byte_end": 30, "message": "missing identifier"},
			},
			"diagnostics": []any{},
		}
	})

	errs := ASTErrors([]byte("package main\nbroken"), "test.go")
	require.Len(t, errs, 2, "ASTErrors must surface EVERY wire error, not just the first")
	assert.Equal(t, uint32(1), errs[0].Line)
	assert.Equal(t, uint32(4), errs[0].Column)
	assert.Equal(t, "syntax error", errs[0].Message)
	assert.Equal(t, uint32(3), errs[1].Line)
	assert.Equal(t, "missing identifier", errs[1].Message)
	assert.Equal(t, "test.go", errs[1].FilePath)
}

func TestASTErrors_ValidGo_ReturnsNil(t *testing.T) {
	useFakeDaemon(t, func(map[string]any) any { return okValidateResp() })
	assert.Nil(t, ASTErrors([]byte("package main\n"), "test.go"))
}

func TestASTErrors_UnknownExtension_ReturnsNil(t *testing.T) {
	useDeadSocket(t)
	assert.Nil(t, ASTErrors([]byte(`broken {{{`), "test.txt"))
}

func TestASTErrors_DaemonUnreachable_ReturnsNil(t *testing.T) {
	// The diagnostic flavor has no error channel — it degrades to nil,
	// matching the historical parse-failure contract.
	useDeadSocket(t)
	assert.Nil(t, ASTErrors([]byte("package main\n"), "test.go"))
}

func TestSupportedPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"main.go", true},
		{"app.py", true},
		{"index.js", true},
		{"index.ts", true},
		{"comp.tsx", true},
		{"lib.rs", true},
		{"app.ex", true},
		{"script.exs", true},
		{"MAIN.GO", true}, // extension matching is case-insensitive
		{"infra.tf", false},
		{"conf.yaml", false},
		{"query.sql", false},
		{"README.md", false},
		{"data.json", false},
		{"no_extension", false},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			assert.Equal(t, tc.want, SupportedPath(tc.path),
				"SupportedPath must agree with the leyline daemon's validate language set")
		})
	}
}
