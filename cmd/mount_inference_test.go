package cmd

import (
	"errors"
	"io/fs"
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
// the source file does not exist. Uses errors.Is(fs.ErrNotExist) so the
// assertion is robust to future error wrapping (e.g. fmt.Errorf("read %s: %w", ...))
// which would still satisfy errors.Is but NOT os.IsNotExist. Path is rooted
// in t.TempDir() so the test does not depend on global filesystem state.
func TestInferFromTreeSitterFile_MissingFile(t *testing.T) {
	goLang := lang.ForExt(".go")
	require.NotNil(t, goLang)
	inf := &lattice.Inferrer{Config: lattice.DefaultInferConfig()}

	missing := filepath.Join(t.TempDir(), "does-not-exist.go")
	_, err := inferFromTreeSitterFile(inf, missing, goLang.Grammar(), goLang.DisplayName)
	require.Error(t, err)
	require.True(t, errors.Is(err, fs.ErrNotExist), "expected fs.ErrNotExist, got %v", err)
}
