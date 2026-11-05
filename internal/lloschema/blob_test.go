package lloschema

import (
	"encoding/hex"
	"testing"

	"github.com/zeebo/blake3"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/fixturedb"
)

// sourceBytes reads one _source row through cols (SourceColumns) and a
// SourceScan.
func sourceBytes(t *testing.T, f *fixturedb.FixtureDB, cols, id string) (*SourceScan, []byte, error) {
	t.Helper()
	var s SourceScan
	require.NoError(t, f.DB().QueryRow("SELECT "+cols+" FROM _source WHERE id = ?", id).Scan(s.Targets()...))
	b, err := s.Bytes()
	return &s, b, err
}

// TestSourceScan_ReadsTheParsedBytes is mache-46c59d. ley-line leaves
// _source.content NULL and keeps the parsed bytes in source_blobs under
// content_hash; mache's cache-hydrated db (Standalone) carries them inline and
// has no source_blobs, where naming that table would fail the statement.
func TestSourceScan_ReadsTheParsedBytes(t *testing.T) {
	for _, p := range []fixturedb.Producer{fixturedb.Leyline, fixturedb.LeylineV6, fixturedb.Standalone} {
		t.Run(p.String(), func(t *testing.T) {
			b := fixturedb.New(t, p)
			b.Source("a.go", "go", "package a\n")
			_, f := b.Build()
			cols, cerr := SourceColumns(f)
			require.NoError(t, cerr)
			_, got, err := sourceBytes(t, f, cols, "a.go")
			require.NoError(t, err)
			assert.Equal(t, []byte("package a\n"), got)
		})
	}
}

// TestSourceScan_NoBytesIsRefused: a source the projection holds no bytes for
// is an error, which callers surface; nothing reads the file in its place.
func TestSourceScan_NoBytesIsRefused(t *testing.T) {
	for _, p := range []fixturedb.Producer{fixturedb.Leyline, fixturedb.Standalone} {
		t.Run(p.String(), func(t *testing.T) {
			b := fixturedb.New(t, p)
			b.Source("a.go", "go", "")
			_, f := b.Build()
			cols, cerr := SourceColumns(f)
			require.NoError(t, cerr)
			_, got, err := sourceBytes(t, f, cols, "a.go")
			require.ErrorIs(t, err, ErrNoSourceBytes)
			assert.Nil(t, got)
		})
	}
}

// TestSourceScan_FollowsAReparse: the bytes are whichever blob the row names
// now, so a reparse is seen without anything reading the file.
func TestSourceScan_FollowsAReparse(t *testing.T) {
	b := fixturedb.New(t, fixturedb.Leyline)
	b.Source("a.go", "go", "package a\n")
	_, f := b.Build()
	f.Reparse("a.go", "package a // edited\n")
	cols, cerr := SourceColumns(f)
	require.NoError(t, cerr)
	_, got, err := sourceBytes(t, f, cols, "a.go")
	require.NoError(t, err)
	assert.Equal(t, []byte("package a // edited\n"), got)
}

// TestSourceScan_NamesTheBlob: a leyline row names the blob its bytes are
// (the propose base, mache-631b8b), and the bytes hash to it.
func TestSourceScan_NamesTheBlob(t *testing.T) {
	for _, p := range []fixturedb.Producer{fixturedb.Leyline, fixturedb.LeylineV6} {
		t.Run(p.String(), func(t *testing.T) {
			b := fixturedb.New(t, p)
			b.Source("a.go", "go", "package a\n")
			_, f := b.Build()
			cols, cerr := SourceColumns(f)
			require.NoError(t, cerr)
			s, got, err := sourceBytes(t, f, cols, "a.go")
			require.NoError(t, err)
			assert.Len(t, s.Hash(), 32)
			assert.NoError(t, VerifyBlob(s.Hash(), got), "the hash names exactly the parsed bytes")
		})
	}
}

// blobFixture is a leyline fixture holding a.go, and a.go's blob hash.
func blobFixture(t *testing.T) (*fixturedb.FixtureDB, []byte) {
	t.Helper()
	b := fixturedb.New(t, fixturedb.LeylineV6)
	b.Source("a.go", "go", "package a\n")
	_, f := b.Build()
	var h []byte
	require.NoError(t, f.DB().QueryRow(`SELECT content_hash FROM _source WHERE id = 'a.go'`).Scan(&h))
	return f, h
}

// TestStoredBytes_AreVerifiedOnEveryRead is mache-c1eb7a: bytes changed under
// their hash, bytes moved into chunks (NULL), and a missing row are three
// distinct refusals on every read path, never served. A chunked row is never
// an empty file.
func TestStoredBytes_AreVerifiedOnEveryRead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		damage func(*testing.T, *fixturedb.FixtureDB, []byte)
		want   error
	}{
		{"tampered", func(t *testing.T, f *fixturedb.FixtureDB, h []byte) {
			fixturedb.DamageBlob(t, f.DB(), h, fixturedb.BlobTampered)
		}, ErrBlobTampered},
		{"chunked", func(t *testing.T, f *fixturedb.FixtureDB, h []byte) {
			fixturedb.DamageBlob(t, f.DB(), h, fixturedb.BlobChunked)
		}, ErrBlobChunked},
		{"missing", func(t *testing.T, f *fixturedb.FixtureDB, h []byte) {
			fixturedb.DamageBlob(t, f.DB(), h, fixturedb.BlobDropped)
		}, ErrNoBlob},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, h := blobFixture(t)
			tc.damage(t, f, h)
			cols, cerr := SourceColumns(f)
			require.NoError(t, cerr)
			_, _, err := sourceBytes(t, f, cols, "a.go")
			assert.ErrorIs(t, err, tc.want, "the _source path")
			_, err = ReadBlob(f, h)
			assert.ErrorIs(t, err, tc.want, "ReadBlob")
			if err != nil {
				assert.Contains(t, err.Error(), hex.EncodeToString(h), "the refusal names the hash")
			}
		})
	}
}

// TestReadBlob_EmptyFileIsEmptyNotChunked: a zero-length blob is an empty
// file, verified like any other; only NULL is "held as chunks".
func TestReadBlob_EmptyFileIsEmptyNotChunked(t *testing.T) {
	f, _ := blobFixture(t)
	h := fixturedb.AddBlob(t, f.DB(), []byte{})
	want := blake3.Sum256(nil)
	require.Equal(t, want[:], h, "keyed as leyline keys it")
	got, err := ReadBlob(f, h)
	require.NoError(t, err)
	assert.NotNil(t, got)
	assert.Empty(t, got)
}
