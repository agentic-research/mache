package ingest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/stretchr/testify/require"
)

// TestASTWalker_InvalidateSource_ReReadsAfterChange proves InvalidateSource
// drops the per-file caches so a subsequent query re-reads the db — the hook
// the mount/serve watcher needs so an edit isn't masked by the walker's
// immortal caches (mache-018eee/mache-024e9c).
//
// The `_source` row is path-mode, as ley-line writes it by default, so the
// edit is the one the watcher actually sees: the file on disk changes under
// an unchanged row.
func TestASTWalker_InvalidateSource_ReReadsAfterChange(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "main.go")
	require.NoError(t, os.WriteFile(srcPath, []byte("package main\n"), 0o644))

	b := fixturedb.New(t, fixturedb.Leyline)
	b.SourceFile("main.go", "go", srcPath)
	b.ASTNode("main.go", "source_file", "main.go", fixturedb.Bytes(0, 13))
	_, f := b.Build()
	w := NewASTWalker(f.DB())

	require.Equal(t, "go", w.fileLang("main.go"))
	require.Equal(t, "package main\n", string(w.fileSource("main.go"))) // loads the file's section into indexCache

	// Change the file; the cache still serves the stale bytes.
	require.NoError(t, os.WriteFile(srcPath, []byte("package main\n\n// edited\n"), 0o644))
	require.Equal(t, "package main\n", string(w.fileSource("main.go")), "cache still serves the pre-change value")

	w.InvalidateSource("main.go")
	require.Equal(t, "package main\n\n// edited\n", string(w.fileSource("main.go")), "InvalidateSource forces a re-read")
}
