package validate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The validate package is mache's public API surface for AST
// syntax validation — external consumers (e.g. CI scripts,
// pre-commit hooks) import it instead of internal/writeback. The
// tests here pin the API contract: valid input returns nil, invalid
// input returns a populated error / non-empty error slice, and the
// File/FileErrors variants reach disk correctly. The underlying
// tree-sitter parsing is exercised by internal/writeback's own
// suite; we don't re-test it here.

func TestContent_ValidGoReturnsNil(t *testing.T) {
	src := []byte("package main\n\nfunc main() {}\n")
	assert.NoError(t, Content(src, "main.go"))
}

func TestContent_InvalidGoReturnsError(t *testing.T) {
	src := []byte("package main\n\nfunc main() { syntax error here }\n")
	assert.Error(t, Content(src, "main.go"),
		"validate.Content must surface syntax errors so external CI gates can fail loudly")
}

func TestContent_UnknownExtensionPassesThrough(t *testing.T) {
	// Pass-through guarantee: callers can hand validate any
	// extension without wrapping in a language probe. .xyz has no
	// tree-sitter grammar, so we return nil rather than a "no
	// grammar" error.
	src := []byte("anything goes here\n")
	assert.NoError(t, Content(src, "data.xyz"))
}

func TestContentErrors_ValidGoReturnsEmpty(t *testing.T) {
	src := []byte("package main\n\nfunc main() {}\n")
	errs := ContentErrors(src, "main.go")
	assert.Empty(t, errs)
}

func TestContentErrors_InvalidGoPopulatesSlice(t *testing.T) {
	// The point of ContentErrors over Content is structured
	// per-error positions for diagnostic UIs — assert we get at
	// least one entry, not a count, since tree-sitter's recovery
	// can split one logical error into multiple AST nodes.
	src := []byte("package main\n\nfunc main() { syntax error here }\n")
	errs := ContentErrors(src, "main.go")
	assert.NotEmpty(t, errs,
		"invalid Go source must produce at least one ValidationError for diagnostic rendering")
}

func TestFile_ValidGoReturnsNil(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	require.NoError(t, os.WriteFile(path, []byte("package main\n"), 0o644))
	assert.NoError(t, File(path))
}

func TestFile_InvalidGoReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	require.NoError(t, os.WriteFile(path,
		[]byte("package main\n\nfunc main() { syntax error here }\n"), 0o644))
	assert.Error(t, File(path))
}

func TestFile_NonexistentReturnsError(t *testing.T) {
	// Read errors propagate — callers can distinguish "file
	// missing" from "syntax error" by the error type if they
	// need to (errors.Is on os.ErrNotExist).
	err := File("/nonexistent/path/to/file.go")
	assert.Error(t, err)
}

func TestFileErrors_NonexistentReturnsNil(t *testing.T) {
	// FileErrors swallows read errors and returns nil — the
	// diagnostic UI flavor doesn't have a way to surface a
	// non-AST error. Callers that care about read errors should
	// use File() instead.
	errs := FileErrors("/nonexistent/path/to/file.go")
	assert.Nil(t, errs)
}

func TestSupportedExtension(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"main.go", true},
		{"app.py", true},
		{"index.ts", true},
		{"infra.tf", true},
		{"data.json", false}, // JSON has no tree-sitter grammar in mache's set
		{"README.md", true},  // Markdown is registered
		{"no_extension", false},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			assert.Equal(t, tc.want, SupportedExtension(tc.path),
				"SupportedExtension must agree with the tree-sitter grammar registry")
		})
	}
}
