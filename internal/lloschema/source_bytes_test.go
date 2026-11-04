package lloschema

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/fixturedb"
)

// scanBytes reads one _source row's bytes through expr.
func scanBytes(t *testing.T, f *fixturedb.FixtureDB, expr, id string) []byte {
	t.Helper()
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

			expr, err := SourceBytesExpr(f)
			require.NoError(t, err)
			assert.Equal(t, []byte("package a\n"), scanBytes(t, f, expr, "a.go"))
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

			expr, err := SourceBytesExpr(f)
			require.NoError(t, err)
			assert.Nil(t, scanBytes(t, f, expr, "a.go"))
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

	expr, err := SourceBytesExpr(f)
	require.NoError(t, err)
	assert.Equal(t, []byte("package a // edited\n"), scanBytes(t, f, expr, "a.go"))
}

// TestSourceHashExpr_NamesTheBlob: a leyline row names the blob its bytes are
// (the propose base, mache-631b8b); mache's cache-hydrated _source has no
// content_hash column and names none, without failing the statement.
func TestSourceHashExpr_NamesTheBlob(t *testing.T) {
	for _, p := range []fixturedb.Producer{fixturedb.Leyline, fixturedb.LeylineV6} {
		t.Run(p.String(), func(t *testing.T) {
			b := fixturedb.New(t, p)
			b.Source("a.go", "go", "package a\n")
			_, f := b.Build()
			expr, err := SourceHashExpr(f)
			require.NoError(t, err)

			var hash, blob []byte
			require.NoError(t, f.DB().QueryRow("SELECT "+expr+", b.blob_bytes FROM _source "+
				"JOIN source_blobs b ON b.blob_hash = _source.content_hash WHERE _source.id = 'a.go'").Scan(&hash, &blob))
			assert.Len(t, hash, 32)
			assert.Equal(t, "package a\n", string(blob), "the hash names exactly the parsed bytes")
		})
	}
}
