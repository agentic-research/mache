package lloschema

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/fixturedb"
)

// sourceBytes runs SourceBytesExpr against one _source row of f.
func sourceBytes(t *testing.T, f *fixturedb.FixtureDB, id string) []byte {
	t.Helper()
	expr, err := SourceBytesExpr(f)
	require.NoError(t, err)
	var got []byte
	require.NoError(t, f.DB().QueryRow("SELECT "+expr+" FROM _source WHERE id = ?", id).Scan(&got))
	return got
}

// TestSourceBytesExpr_ReadsTheParsedBytes is mache-46c59d. ley-line leaves
// _source.content NULL and keeps the parsed bytes in source_blobs under
// content_hash; mache's cache-hydrated db (Standalone) carries them inline and
// has no source_blobs, where naming that table would fail the statement.
func TestSourceBytesExpr_ReadsTheParsedBytes(t *testing.T) {
	for _, p := range []fixturedb.Producer{fixturedb.Leyline, fixturedb.LeylineV6, fixturedb.Standalone} {
		t.Run(p.String(), func(t *testing.T) {
			b := fixturedb.New(t, p)
			b.Source("a.go", "go", "package a\n")
			_, f := b.Build()

			assert.Equal(t, []byte("package a\n"), sourceBytes(t, f, "a.go"))
		})
	}
}

// TestSourceBytesExpr_NoBytesIsNull: a source the projection holds no bytes for
// reads as NULL, which callers refuse; nothing reads the file in its place.
func TestSourceBytesExpr_NoBytesIsNull(t *testing.T) {
	for _, p := range []fixturedb.Producer{fixturedb.Leyline, fixturedb.Standalone} {
		t.Run(p.String(), func(t *testing.T) {
			b := fixturedb.New(t, p)
			b.Source("a.go", "go", "")
			_, f := b.Build()

			assert.Nil(t, sourceBytes(t, f, "a.go"))
		})
	}
}

// TestSourceBytesExpr_FollowsAReparse: the bytes are whichever blob the row
// names now, so a reparse is seen without anything reading the file.
func TestSourceBytesExpr_FollowsAReparse(t *testing.T) {
	b := fixturedb.New(t, fixturedb.Leyline)
	b.Source("a.go", "go", "package a\n")
	_, f := b.Build()
	f.Reparse("a.go", "package a // edited\n")

	assert.Equal(t, []byte("package a // edited\n"), sourceBytes(t, f, "a.go"))
}
