package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/internal/lang"
	"github.com/agentic-research/mache/internal/lattice"
	"github.com/stretchr/testify/require"
)

// TestInferFromTreeSitterFile_GoSource verifies that the helper parses a real
// Go source file and produces a topology — exercises the file-read + parse
// + Inferrer hand-off path that the --infer CLI flag depends on.
func TestInferFromTreeSitterFile_GoSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.go")
	src := []byte("package sample\n\nfunc Hello() string { return \"hi\" }\n\nfunc World() string { return \"world\" }\n")
	require.NoError(t, os.WriteFile(path, src, 0o644))

	goLang := lang.ForExt(".go")
	require.NotNil(t, goLang, "Go language must be registered")

	inf := &lattice.Inferrer{Config: lattice.DefaultInferConfig()}
	topo, err := inferFromTreeSitterFile(inf, path, goLang.Grammar(), goLang.DisplayName)
	require.NoError(t, err)
	require.NotNil(t, topo)
	// Inferred topology should have a non-empty version field set by the inferrer.
	// We do not assert on schema shape — that belongs to the inferrer's own
	// tests; here we only assert the wiring works end-to-end.
}

// TestInferFromTreeSitterFile_MissingFile returns the wrapped os error when
// the source file does not exist.
func TestInferFromTreeSitterFile_MissingFile(t *testing.T) {
	goLang := lang.ForExt(".go")
	require.NotNil(t, goLang)
	inf := &lattice.Inferrer{Config: lattice.DefaultInferConfig()}

	_, err := inferFromTreeSitterFile(inf, "/nonexistent/path/does-not-exist.go", goLang.Grammar(), goLang.DisplayName)
	require.Error(t, err)
	require.True(t, os.IsNotExist(err), "expected fs.ErrNotExist, got %v", err)
}
