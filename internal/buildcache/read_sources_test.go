package buildcache

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/fixturedb"
)

// TestReadSources_ReadsTheParsedBytes is mache-46c59d. A chunk carries the
// bytes beside the offsets it was keyed by, so they must be the parsed bytes
// (source_blobs on a ley-line db), not the file at _source.path, which may
// have changed since the parse or, as here, not exist at all.
func TestReadSources_ReadsTheParsedBytes(t *testing.T) {
	for _, p := range []fixturedb.Producer{fixturedb.Leyline, fixturedb.LeylineV6, fixturedb.Standalone} {
		t.Run(p.String(), func(t *testing.T) {
			b := fixturedb.New(t, p)
			b.Source("a.go", "go", "package a\n")
			_, f := b.Build()

			rows, err := readSources(f.DB())

			require.NoError(t, err)
			require.Len(t, rows, 1)
			assert.Equal(t, "package a\n", string(rows[0].content))
		})
	}
}

// TestReadSources_RefusesASourceWithoutBytes: a chunk missing a source would be
// an incomplete cache entry, so a row the projection holds no bytes for is an
// error rather than skipped or read from disk.
func TestReadSources_RefusesASourceWithoutBytes(t *testing.T) {
	b := fixturedb.New(t, fixturedb.Leyline)
	b.Source("a.go", "go", "package a\n")
	b.Source("b.go", "go", "")
	_, f := b.Build()

	_, err := readSources(f.DB())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "b.go")
}
