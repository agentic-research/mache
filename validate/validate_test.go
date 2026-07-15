package validate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/lltest"
)

// The validate package is mache's public API surface for syntax validation —
// external consumers (e.g. CI scripts, pre-commit hooks) import it instead of
// internal/writeback. The tests here pin the API contract: valid input
// returns nil, invalid input returns a populated error / non-empty error
// slice, and the File/FileErrors variants reach disk correctly. Validation is
// served by a leyline daemon; these tests run against a fake daemon whose
// verdict keys off a content marker, so the suite needs no real binary. The
// real-parser path is covered by internal/writeback's gated e2e suite.

// brokenMarker makes the fake daemon report a syntax error — the tests only
// exercise plumbing, not parsing.
const brokenMarker = "syntax error here"

// useMarkerDaemon wires a fake leyline daemon that flags any content
// containing brokenMarker and validates everything else.
func useMarkerDaemon(t *testing.T) {
	t.Helper()
	sock := lltest.FakeDaemon(t, func(req map[string]any) any {
		content, _ := req["content"].(string)
		if strings.Contains(content, brokenMarker) {
			return map[string]any{
				"ok": false,
				"errors": []any{
					map[string]any{"row": 2, "col": 14, "byte_start": 28, "byte_end": 45, "message": "syntax error"},
				},
				"diagnostics": []any{},
			}
		}
		return map[string]any{"ok": true, "errors": []any{}, "diagnostics": []any{}}
	})
	t.Setenv("LEYLINE_SOCKET", sock)
}

func TestContent_ValidGoReturnsNil(t *testing.T) {
	useMarkerDaemon(t)
	src := []byte("package main\n\nfunc main() {}\n")
	assert.NoError(t, Content(src, "main.go"))
}

func TestContent_InvalidGoReturnsError(t *testing.T) {
	useMarkerDaemon(t)
	src := []byte("package main\n\nfunc main() { syntax error here }\n")
	assert.Error(t, Content(src, "main.go"),
		"validate.Content must surface syntax errors so external CI gates can fail loudly")
}

func TestContent_UnknownExtensionPassesThrough(t *testing.T) {
	// Pass-through guarantee: callers can hand validate any extension
	// without wrapping in a language probe. .xyz is not in the daemon's
	// validate set, so we return nil rather than a "no grammar" error —
	// and the daemon is never contacted (no fake is wired here).
	src := []byte("anything goes here\n")
	assert.NoError(t, Content(src, "data.xyz"))
}

func TestContentErrors_ValidGoReturnsEmpty(t *testing.T) {
	useMarkerDaemon(t)
	src := []byte("package main\n\nfunc main() {}\n")
	errs := ContentErrors(src, "main.go")
	assert.Empty(t, errs)
}

func TestContentErrors_InvalidGoPopulatesSlice(t *testing.T) {
	// The point of ContentErrors over Content is structured per-error
	// positions for diagnostic UIs — assert we get at least one entry.
	useMarkerDaemon(t)
	src := []byte("package main\n\nfunc main() { syntax error here }\n")
	errs := ContentErrors(src, "main.go")
	assert.NotEmpty(t, errs,
		"invalid Go source must produce at least one ValidationError for diagnostic rendering")
}

func TestFile_ValidGoReturnsNil(t *testing.T) {
	useMarkerDaemon(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	require.NoError(t, os.WriteFile(path, []byte("package main\n"), 0o644))
	assert.NoError(t, File(path))
}

func TestFile_InvalidGoReturnsError(t *testing.T) {
	useMarkerDaemon(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	require.NoError(t, os.WriteFile(path,
		[]byte("package main\n\nfunc main() { syntax error here }\n"), 0o644))
	assert.Error(t, File(path))
}

func TestFile_NonexistentReturnsError(t *testing.T) {
	// Read errors propagate — callers can distinguish "file missing" from
	// "syntax error" by the error type if they need to (errors.Is on
	// os.ErrNotExist).
	err := File("/nonexistent/path/to/file.go")
	assert.Error(t, err)
}

func TestFileErrors_NonexistentReturnsNil(t *testing.T) {
	// FileErrors swallows read errors and returns nil — the diagnostic UI
	// flavor doesn't have a way to surface a non-AST error. Callers that
	// care about read errors should use File() instead.
	errs := FileErrors("/nonexistent/path/to/file.go")
	assert.Nil(t, errs)
}

func TestSupportedExtension(t *testing.T) {
	// Since mache-73b885 the supported set is the leyline daemon's validate
	// language set — .tf/.hcl, .sql, .yaml, .md, .toml (covered by the old
	// in-process grammar set) are now pass-through, .ex/.exs are new.
	tests := []struct {
		path string
		want bool
	}{
		{"main.go", true},
		{"app.py", true},
		{"index.ts", true},
		{"comp.tsx", true},
		{"lib.rs", true},
		{"app.ex", true},
		{"infra.tf", false},  // was true pre-mache-73b885
		{"README.md", false}, // was true pre-mache-73b885
		{"data.json", false},
		{"no_extension", false},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			assert.Equal(t, tc.want, SupportedExtension(tc.path),
				"SupportedExtension must agree with the leyline validate language set")
		})
	}
}
