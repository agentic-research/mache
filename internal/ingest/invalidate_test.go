package ingest

import (
	"testing"

	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/stretchr/testify/require"
)

// TestASTWalker_InvalidateSource_ReReadsAfterChange proves InvalidateSource
// drops the per-file caches so a subsequent query re-reads the db — the hook
// the mount/serve watcher needs so an edit isn't masked by the walker's
// immortal caches (mache-018eee/mache-024e9c).
//
// The edit lands where ley-line puts it, in the projection (a reparse writes a
// new source_blobs row), since the walker reads the parsed bytes and never the
// file on disk (mache-46c59d).
func TestASTWalker_InvalidateSource_ReReadsAfterChange(t *testing.T) {
	b := fixturedb.New(t, fixturedb.Leyline)
	b.Source("main.go", "go", "package main\n")
	b.ASTNode("main.go", "source_file", "main.go", fixturedb.Bytes(0, 13))
	_, f := b.Build()
	w := NewASTWalker(f.DB())

	require.Equal(t, "go", w.fileLang("main.go"))
	require.Equal(t, "package main\n", string(w.fileSource("main.go"))) // loads the file's section into indexCache

	// Reparse the edited file; the cache still serves the stale bytes.
	f.Reparse("main.go", "package main\n\n// edited\n")
	require.Equal(t, "package main\n", string(w.fileSource("main.go")), "cache still serves the pre-change value")

	w.InvalidateSource("main.go")
	require.Equal(t, "package main\n\n// edited\n", string(w.fileSource("main.go")), "InvalidateSource forces a re-read")
}
