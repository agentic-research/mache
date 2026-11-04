package graph_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/fixturedb"
)

// TestNewParseBlobs_ServesParsedAndAcceptedBytes: a write's base is either a
// parsed blob (source_blobs, by _source.content_hash) or the bytes of mache's
// own last accepted write to that file; a newer write replaces the older one.
func TestNewParseBlobs_ServesParsedAndAcceptedBytes(t *testing.T) {
	b := fixturedb.New(t, fixturedb.Leyline)
	b.Source("a.go", "go", "package a\n")
	_, f := b.Build()
	var parsed []byte
	require.NoError(t, f.DB().QueryRow(`SELECT content_hash FROM _source WHERE id = 'a.go'`).Scan(&parsed))
	p := graph.NewParseBlobs(f.DB())

	got, ok := p.Blob(parsed)
	require.True(t, ok)
	assert.Equal(t, "package a\n", string(got))
	_, ok = p.Blob([]byte("unknown"))
	assert.False(t, ok)
	_, ok = p.Blob(nil)
	assert.False(t, ok)

	p.Remember("/x/a.go", []byte("h1"), []byte("first"))
	p.Remember("/x/a.go", []byte("h2"), []byte("second"))
	_, ok = p.Blob([]byte("h1"))
	assert.False(t, ok, "only a file's latest accepted bytes can be a base")
	got, ok = p.Blob([]byte("h2"))
	require.True(t, ok)
	assert.Equal(t, "second", string(got))
}
