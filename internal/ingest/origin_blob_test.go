package ingest

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/fixturedb"
)

// TestIngest_OriginNamesTheParsedBlob is mache-631b8b, against the pinned
// leyline. A construct's origin offsets refer to one parsed blob, and the
// origin names it: the _source.content_hash that propose checks a write's base
// against, so a write can never apply these offsets to other bytes.
func TestIngest_OriginNamesTheParsedBlob(t *testing.T) {
	dir := t.TempDir()
	rs := filepath.Join(dir, "lib.rs")
	require.NoError(t, os.WriteFile(rs, []byte("pub fn alpha() {}\n\npub fn beta() {}\n"), 0o644))
	dbPath := parseWithLeyline(t, dir)
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	store := graph.NewMemoryStore()
	engine := NewEngine(fnPerConstructSchema(), store)
	engine.SetASTWalker(NewASTWalker(db))
	require.NoError(t, engine.Ingest(dir))

	var want []byte
	require.NoError(t, db.QueryRow(`SELECT content_hash FROM _source WHERE path LIKE '%lib.rs'`).Scan(&want))
	require.Len(t, want, 32, "leyline names every parsed file's blob")
	for _, id := range []string{"fns/alpha/source", "fns/beta/source"} {
		n, err := store.GetNode(id)
		require.NoError(t, err)
		require.NotNil(t, n.Origin, id)
		assert.Equal(t, want, n.Origin.Blob, "%s names the blob its offsets refer to", id)
	}
}

// TestASTWalker_FileBlob_IsTheRowsHash: a file's section names the hash of
// exactly the bytes it holds, follows a reparse once the file's caches are
// dropped, and is nil on mache's cache-hydrated db, which records no hash.
func TestASTWalker_FileBlob_IsTheRowsHash(t *testing.T) {
	const src = "package main\n\nfunc F() {}\n"
	build := func(p fixturedb.Producer) (*fixturedb.FixtureDB, *ASTWalker) {
		b := fixturedb.New(t, p)
		b.Source("main.go", "go", src)
		b.ASTNode("main.go", "source_file", "main.go", fixturedb.Bytes(0, len(src)))
		_, f := b.Build()
		return f, NewASTWalker(f.DB())
	}
	hashOf := func(f *fixturedb.FixtureDB) []byte {
		var h []byte
		require.NoError(t, f.DB().QueryRow(`SELECT content_hash FROM _source WHERE id = 'main.go'`).Scan(&h))
		return h
	}

	f, w := build(fixturedb.Leyline)
	require.NotNil(t, hashOf(f))
	assert.Equal(t, hashOf(f), blobOf(w, "main.go"))

	f.Reparse("main.go", src+"// edited\n")
	w.InvalidateSource("main.go")
	assert.Equal(t, hashOf(f), blobOf(w, "main.go"), "a reparse names a new blob")

	_, ws := build(fixturedb.Standalone)
	assert.Nil(t, blobOf(ws, "main.go"),
		"no recorded hash names no blob; the write path refuses rather than guess")
}

// blobOf is the blob stampOriginBlob would name for sourceID: none when the
// walker holds no section for it.
func blobOf(w *ASTWalker, sourceID string) []byte {
	if idx := w.sectionOrNil(sourceID); idx != nil {
		return idx.blob
	}
	return nil
}
