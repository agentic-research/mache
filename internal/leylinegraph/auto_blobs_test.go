package leylinegraph

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/lltest"
)

// TestAttachLeylineASTWalker_GivesTheStoreItsBlobs: the store a source
// projection is built into reads its writes' base bytes from the same parse
// db, so every origin's Blob resolves to the bytes its offsets were taken
// from (mache-3a0fb9).
func TestAttachLeylineASTWalker_GivesTheStoreItsBlobs(t *testing.T) {
	lltest.ResolveBinaryOrSkip(t)
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	const src = "package p\n\nfunc F() {}\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "p.go"), []byte(src), 0o644))
	schema := &api.Topology{Version: "v1", Nodes: []api.Node{{
		Name: "fns", Selector: "$", Language: "go",
		Children: []api.Node{{
			Name:     "{{.name}}",
			Selector: "(function_declaration name: (identifier) @name) @scope",
			Files:    []api.Leaf{{Name: "source", ContentTemplate: "{{.scope}}"}},
		}},
	}}}
	store := graph.NewMemoryStore()
	engine := ingest.NewEngine(schema, store)

	_, cleanup, err := AttachLeylineASTWalker(dir, engine)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	require.NoError(t, engine.Ingest(dir))

	require.NotNil(t, store.Blobs())
	n, err := store.GetNode("fns/F/source")
	require.NoError(t, err)
	got, ok := store.Blobs().Blob(n.Origin.Blob)
	require.True(t, ok, "the origin's blob is one the store can read")
	assert.Equal(t, src, string(got))
}
