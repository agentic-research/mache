package leylinegraph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/fixturedb"
)

// TestWireParseDB_GivesTheStoreItsBlobs: the store a source projection builds
// reads the parse db's blobs for its writes.
func TestWireParseDB_GivesTheStoreItsBlobs(t *testing.T) {
	b := fixturedb.New(t, fixturedb.Leyline)
	b.Source("a.go", "go", "package a\n")
	_, f := b.Build()
	store := graph.NewMemoryStore()

	WireParseDB(store, f.DB())

	require.NotNil(t, store.Blobs())
	var h []byte
	require.NoError(t, f.DB().QueryRow(`SELECT content_hash FROM _source`).Scan(&h))
	got, ok := store.Blobs().Blob(h)
	require.True(t, ok)
	assert.Equal(t, "package a\n", string(got))
}
