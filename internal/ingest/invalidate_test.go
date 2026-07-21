package ingest

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestASTWalker_InvalidateSource_ReReadsAfterChange proves InvalidateSource
// drops the per-file caches so a subsequent query re-reads the db — the hook
// the mount/serve watcher needs so an edit isn't masked by the walker's
// immortal caches (mache-018eee/mache-024e9c).
func TestASTWalker_InvalidateSource_ReReadsAfterChange(t *testing.T) {
	db := seedTestAST(t)
	defer func() { _ = db.Close() }()
	w := NewASTWalker(db)

	require.Equal(t, "go", w.fileLang("main.go")) // populates langCache

	// Change the underlying row; the cache still serves the stale value.
	_, err := db.Exec("UPDATE _source SET language='python' WHERE id='main.go'")
	require.NoError(t, err)
	require.Equal(t, "go", w.fileLang("main.go"), "cache still serves the pre-change value")

	w.InvalidateSource("main.go")
	require.Equal(t, "python", w.fileLang("main.go"), "InvalidateSource forces a re-read")
}
