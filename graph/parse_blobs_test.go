package graph_test

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/agentic-research/mache/internal/lloschema"
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

	got, err := p.Blob(parsed)
	require.NoError(t, err)
	assert.Equal(t, "package a\n", string(got))
	_, err = p.Blob([]byte("unknown"))
	assert.ErrorIs(t, err, lloschema.ErrNoBlob)
	_, err = p.Blob(nil)
	assert.ErrorIs(t, err, lloschema.ErrNoBlob)

	p.Remember("/x/a.go", []byte("h1"), []byte("first"))
	p.Remember("/x/a.go", []byte("h2"), []byte("second"))
	_, err = p.Blob([]byte("h1"))
	assert.ErrorIs(t, err, lloschema.ErrNoBlob, "only a file's latest accepted bytes can be a base")
	got, err = p.Blob([]byte("h2"))
	require.NoError(t, err)
	assert.Equal(t, "second", string(got))
}

// TestNewParseBlobs_RefusesTamperedBytes: parsed bytes changed under their
// hash are refused, naming the hash, never served as a write's base
// (mache-c1eb7a).
func TestNewParseBlobs_RefusesTamperedBytes(t *testing.T) {
	b := fixturedb.New(t, fixturedb.Leyline)
	b.Source("a.go", "go", "package a\n")
	_, f := b.Build()
	var parsed []byte
	require.NoError(t, f.DB().QueryRow(`SELECT content_hash FROM _source WHERE id = 'a.go'`).Scan(&parsed))
	_, err := f.DB().Exec(`UPDATE source_blobs SET blob_bytes = CAST('package b' AS BLOB) WHERE blob_hash = ?`, parsed)
	require.NoError(t, err)

	got, err := graph.NewParseBlobs(f.DB()).Blob(parsed)
	require.ErrorIs(t, err, lloschema.ErrBlobTampered)
	assert.Contains(t, err.Error(), hex.EncodeToString(parsed))
	assert.Nil(t, got)
}
