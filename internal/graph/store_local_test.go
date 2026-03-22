package graph

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLocalStore_PutGetDelete(t *testing.T) {
	dir := t.TempDir()
	store := NewLocalStore(dir)
	ctx := context.Background()
	repo := "github.com/org/repo"
	data := []byte("sqlite db contents here")

	// Put
	err := store.Put(ctx, repo, 1, bytes.NewReader(data))
	require.NoError(t, err)

	// Head
	meta, err := store.Head(ctx, repo)
	require.NoError(t, err)
	assert.Equal(t, repo, meta.Repo)
	assert.Equal(t, uint64(1), meta.Generation)
	assert.Equal(t, int64(len(data)), meta.Size)

	// Get
	rc, gen, err := store.Get(ctx, repo)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), gen)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	_ = rc.Close()
	assert.Equal(t, data, got)

	// List
	metas, err := store.List(ctx)
	require.NoError(t, err)
	require.Len(t, metas, 1)
	assert.Equal(t, repo, metas[0].Repo)

	// Update (new generation)
	data2 := []byte("updated db")
	err = store.Put(ctx, repo, 2, bytes.NewReader(data2))
	require.NoError(t, err)

	meta, err = store.Head(ctx, repo)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), meta.Generation)

	// Delete
	err = store.Delete(ctx, repo)
	require.NoError(t, err)

	_, err = store.Head(ctx, repo)
	assert.Error(t, err)
}

func TestLocalStore_GetNotFound(t *testing.T) {
	dir := t.TempDir()
	store := NewLocalStore(dir)

	_, _, err := store.Get(context.Background(), "nonexistent")
	assert.Error(t, err)
	var notCached *ErrGraphNotCached
	assert.ErrorAs(t, err, &notCached)
}

func TestLocalStore_ListEmpty(t *testing.T) {
	dir := t.TempDir()
	store := NewLocalStore(dir)

	metas, err := store.List(context.Background())
	require.NoError(t, err)
	assert.Empty(t, metas)
}

func TestLocalStore_ListNonexistentDir(t *testing.T) {
	store := NewLocalStore("/tmp/nonexistent-store-dir-" + t.Name())
	defer func() { _ = os.RemoveAll(store.BaseDir) }()

	metas, err := store.List(context.Background())
	require.NoError(t, err)
	assert.Nil(t, metas)
}
