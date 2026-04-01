package graph

import (
	"fmt"
	"io/fs"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// AddFileChildren — bead mache-07e5b4
//
// Contract: atomically add file nodes and append their IDs to the parent's
// Children slice under a single write lock. No intermediate state observable.
// ---------------------------------------------------------------------------

func TestMemoryStore_AddFileChildren_Basic(t *testing.T) {
	store := NewMemoryStore()

	dir := &Node{
		ID:       "pkg/auth",
		Mode:     fs.ModeDir,
		Children: []string{},
	}
	store.AddNode(dir)

	files := []*Node{
		{ID: "pkg/auth/source", Mode: 0, Data: []byte("func Validate() {}")},
		{ID: "pkg/auth/doc", Mode: 0, Data: []byte("// Validate checks auth")},
	}

	store.AddFileChildren(dir, files)

	got, err := store.ListChildren("pkg/auth")
	require.NoError(t, err)
	assert.Equal(t, []string{"pkg/auth/source", "pkg/auth/doc"}, got)

	for _, f := range files {
		n, err := store.GetNode(f.ID)
		require.NoError(t, err)
		assert.Equal(t, f.Data, n.Data)
	}
}

func TestMemoryStore_AddFileChildren_Empty(t *testing.T) {
	store := NewMemoryStore()
	dir := &Node{ID: "pkg/empty", Mode: fs.ModeDir}
	store.AddNode(dir)

	store.AddFileChildren(dir, nil)

	got, err := store.ListChildren("pkg/empty")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestMemoryStore_AddFileChildren_AppendsToExisting(t *testing.T) {
	store := NewMemoryStore()
	dir := &Node{
		ID:       "pkg/util",
		Mode:     fs.ModeDir,
		Children: []string{"pkg/util/existing"},
	}
	store.AddNode(dir)
	store.AddNode(&Node{ID: "pkg/util/existing", Mode: 0, Data: []byte("old")})

	newFiles := []*Node{
		{ID: "pkg/util/source", Mode: 0, Data: []byte("new")},
	}
	store.AddFileChildren(dir, newFiles)

	got, err := store.ListChildren("pkg/util")
	require.NoError(t, err)
	assert.Equal(t, []string{"pkg/util/existing", "pkg/util/source"}, got)
}

func TestMemoryStore_AddFileChildren_Atomicity(t *testing.T) {
	store := NewMemoryStore()
	dir := &Node{ID: "pkg/atomic", Mode: fs.ModeDir}
	store.AddNode(dir)

	files := make([]*Node, 50)
	for i := range files {
		files[i] = &Node{
			ID:   fmt.Sprintf("pkg/atomic/file_%03d", i),
			Mode: 0,
			Data: fmt.Appendf(nil, "content_%d", i),
		}
	}

	// Start barrier: reader starts before writer to maximize overlap window
	ready := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		close(ready) // signal: reader is running
		for k := 0; k < 5000; k++ {
			children, err := store.ListChildren("pkg/atomic")
			if err != nil {
				continue
			}
			n := len(children)
			assert.True(t, n == 0 || n == 50,
				"observed %d children — partial update leaked", n)
		}
	}()

	<-ready // wait for reader goroutine to start
	store.AddFileChildren(dir, files)
	<-done
}

// ---------------------------------------------------------------------------
// ListChildNodes — bead mache-07bbf7
//
// Contract: return []*Node for all children under a single RLock.
// Eliminates N individual GetNode calls during readdir.
// ---------------------------------------------------------------------------

func TestMemoryStore_ListChildNodes_Basic(t *testing.T) {
	store := NewMemoryStore()
	store.AddRoot(&Node{
		ID:       "vulns",
		Mode:     fs.ModeDir,
		Children: []string{"vulns/CVE-1", "vulns/CVE-2"},
	})
	store.AddNode(&Node{ID: "vulns/CVE-1", Mode: fs.ModeDir})
	store.AddNode(&Node{ID: "vulns/CVE-2", Mode: 0, Data: []byte("data")})

	nodes, err := store.ListChildNodes("vulns")
	require.NoError(t, err)
	require.Len(t, nodes, 2)

	ids := make([]string, len(nodes))
	for i, n := range nodes {
		ids[i] = n.ID
	}
	assert.Contains(t, ids, "vulns/CVE-1")
	assert.Contains(t, ids, "vulns/CVE-2")
}

func TestMemoryStore_ListChildNodes_Root(t *testing.T) {
	store := NewMemoryStore()
	store.AddRoot(&Node{ID: "a", Mode: fs.ModeDir})
	store.AddRoot(&Node{ID: "b", Mode: fs.ModeDir})

	nodes, err := store.ListChildNodes("/")
	require.NoError(t, err)
	assert.Len(t, nodes, 2)
}

func TestMemoryStore_ListChildNodes_Empty(t *testing.T) {
	store := NewMemoryStore()
	store.AddNode(&Node{ID: "empty", Mode: fs.ModeDir})

	nodes, err := store.ListChildNodes("empty")
	require.NoError(t, err)
	assert.Empty(t, nodes)
}

func TestMemoryStore_ListChildNodes_NotFound(t *testing.T) {
	store := NewMemoryStore()

	_, err := store.ListChildNodes("nonexistent")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestMemoryStore_ListChildNodes_SkipsMissing(t *testing.T) {
	store := NewMemoryStore()
	store.AddRoot(&Node{
		ID:       "pkg",
		Mode:     fs.ModeDir,
		Children: []string{"pkg/exists", "pkg/ghost"},
	})
	store.AddNode(&Node{ID: "pkg/exists", Mode: 0, Data: []byte("yes")})

	nodes, err := store.ListChildNodes("pkg")
	require.NoError(t, err)
	assert.Len(t, nodes, 1)
	assert.Equal(t, "pkg/exists", nodes[0].ID)
}

func TestMemoryStore_ListChildNodes_SingleRLock(t *testing.T) {
	store := NewMemoryStore()
	dir := &Node{ID: "concurrent", Mode: fs.ModeDir, Children: []string{}}
	store.AddRoot(dir)

	for i := 0; i < 100; i++ {
		id := fmt.Sprintf("concurrent/child_%03d", i)
		store.AddNode(&Node{ID: id, Mode: 0, Data: []byte("x")})
		dir.Children = append(dir.Children, id)
	}
	store.AddNode(dir)

	done := make(chan struct{})
	go func() {
		defer close(done)
		nodes, err := store.ListChildNodes("concurrent")
		assert.NoError(t, err)
		assert.Len(t, nodes, 100)
	}()

	select {
	case <-done:
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("ListChildNodes deadlocked — likely taking multiple locks")
	}
}
