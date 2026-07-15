package cmd

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/internal/lang"
	"github.com/agentic-research/mache/internal/lattice"
	"github.com/agentic-research/mache/internal/leyline"
	"github.com/stretchr/testify/require"
)

// TestInferFromSourceFile_GoSource verifies that the helper leyline-parses a
// real Go source file and produces a topology — exercises the file-stage +
// leyline parse + Inferrer hand-off path that the --infer CLI flag depends
// on. Skips when the pinned leyline binary is unavailable (never downloads
// in tests).
func TestInferFromSourceFile_GoSource(t *testing.T) {
	if _, err := leyline.ResolveBinary(false); err != nil {
		t.Skipf("pinned leyline not available without download: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "sample.go")
	src := []byte("package sample\n\nfunc Hello() string { return \"hi\" }\n\nfunc World() string { return \"world\" }\n")
	require.NoError(t, os.WriteFile(path, src, 0o644))

	goLang := lang.ForExt(".go")
	require.NotNil(t, goLang, "Go language must be registered")

	inf := &lattice.Inferrer{Config: lattice.DefaultInferConfig()}
	topo, err := inferFromSourceFile(inf, path, goLang)
	require.NoError(t, err)
	require.NotNil(t, topo)
	// We do not assert on schema shape — that belongs to the inferrer's own
	// tests; here we only assert the wiring works end-to-end.

	// The Language override must be restored after the call.
	require.Equal(t, lattice.DefaultInferConfig().Language, inf.Config.Language)
}

// TestInferFromSourceFile_MissingFile returns the wrapped os error when
// the source file does not exist. Uses errors.Is(fs.ErrNotExist) so the
// assertion is robust to future error wrapping. The read fails before any
// leyline invocation, so this needs no binary and never skips.
func TestInferFromSourceFile_MissingFile(t *testing.T) {
	goLang := lang.ForExt(".go")
	require.NotNil(t, goLang)
	inf := &lattice.Inferrer{Config: lattice.DefaultInferConfig()}

	missing := filepath.Join(t.TempDir(), "does-not-exist.go")
	_, err := inferFromSourceFile(inf, missing, goLang)
	require.Error(t, err)
	require.True(t, errors.Is(err, fs.ErrNotExist), "expected fs.ErrNotExist, got %v", err)
}
