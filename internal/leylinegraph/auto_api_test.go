package leylinegraph

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAttachLeylineASTWalker_EndToEnd pins the R3-re-homed pipeline through
// the exported entry: parse a real tree with the pinned leyline, wire the
// walker, and hand back a queryable _ast db plus a cleanup that actually
// removes the temp artifacts. Skips (loudly, via RequirePinnedLeyline) when
// the pinned binary is not resolvable offline.
func TestAttachLeylineASTWalker_EndToEnd(t *testing.T) {
	testutil.RequirePinnedLeyline(t)

	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "main.go"),
		[]byte("package main\n\nfunc main() { helper() }\n\nfunc helper() {}\n"), 0o644))

	engine := ingest.NewEngine(&api.Topology{Version: api.SchemaVersion}, graph.NewMemoryStore())
	db, cleanup, err := AttachLeylineASTWalker(src, engine)
	require.NoError(t, err)
	require.NotNil(t, db)

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM _ast`).Scan(&n))
	assert.Positive(t, n, "the walker's db must carry parsed _ast rows")

	var dbFile string
	require.NoError(t, db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&dbFile))
	cleanup()
	_, statErr := os.Stat(dbFile)
	assert.True(t, os.IsNotExist(statErr), "cleanup must remove the temp .db")
}

// TestNoopCallExtractor pins the honest-empty contract: no calls, no error,
// for any input — the answer a data-only backend must give now that there is
// no in-process parser to fall back to (ADR-0012 step 4).
func TestNoopCallExtractor(t *testing.T) {
	ex := NoopCallExtractor()
	calls, err := ex([]byte("func main() { helper() }"), "main.go", "go")
	require.NoError(t, err)
	assert.Empty(t, calls, "a noop extractor must never invent calls")
}
