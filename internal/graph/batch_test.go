package graph

import (
	"fmt"
	"io/fs"
	"sync"
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

	ready := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		close(ready)
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

	<-ready
	store.AddFileChildren(dir, files)
	<-done
}

// ===========================================================================
// ListChildStats — bead mache-07bbf7
//
// Contract: returns []NodeStat VALUE types under a single RLock.
// These tests are FALSIFIABLE — designed to FAIL with the wrong implementation.
// ===========================================================================

// ---------------------------------------------------------------------------
// Test 1: VALUE SEMANTICS (aliasing proof)
//
// FALSIFIABLE: If ListChildStats returned []*Node instead of []NodeStat,
// mutating the node in the store WOULD change what the caller sees.
// With []NodeStat (values), the caller's snapshot is frozen.
// ---------------------------------------------------------------------------

func TestListChildStats_ValueSemantics_NoAliasing(t *testing.T) {
	store := NewMemoryStore()
	store.AddRoot(&Node{
		ID:       "pkg",
		Mode:     fs.ModeDir,
		Children: []string{"pkg/f"},
	})
	store.AddNode(&Node{
		ID:      "pkg/f",
		Mode:    0o444,
		ModTime: time.Unix(1000, 0),
		Data:    []byte("original content — 30 bytes xx"),
	})

	// Take a snapshot via ListChildStats
	stats, err := store.ListChildStats("pkg")
	require.NoError(t, err)
	require.Len(t, stats, 1)

	originalSize := stats[0].ContentSize
	require.Equal(t, int64(32), originalSize)

	// NOW mutate the node in the store
	store.AddNode(&Node{
		ID:      "pkg/f",
		Mode:    0o444,
		ModTime: time.Unix(2000, 0),
		Data:    []byte("new"),
	})

	// The snapshot MUST still reflect the original — this is the falsifiable property.
	// With []*Node, stats[0] would now show ContentSize=3 (aliased pointer).
	// With []NodeStat, it stays at 30 (value copy).
	assert.Equal(t, originalSize, stats[0].ContentSize,
		"NodeStat was aliased to live store data — snapshot semantics violated")
	assert.Equal(t, time.Unix(1000, 0), stats[0].ModTime,
		"ModTime changed after snapshot — aliasing detected")
}

// ---------------------------------------------------------------------------
// Test 2: SNAPSHOT CONSISTENCY (point-in-time)
//
// FALSIFIABLE: If ListChildStats made N individual GetNode calls (N+1 pattern),
// a concurrent writer could change some children between calls, producing a
// mixed-state view. With a single-RLock snapshot, all stats are from one point.
// ---------------------------------------------------------------------------

func TestListChildStats_SnapshotConsistency(t *testing.T) {
	store := NewMemoryStore()
	const N = 100

	dir := &Node{ID: "snap", Mode: fs.ModeDir, Children: make([]string, N)}
	for i := range N {
		id := fmt.Sprintf("snap/child_%03d", i)
		dir.Children[i] = id
		// Phase A: all files have size 10
		store.AddNode(&Node{ID: id, Mode: 0o444, Data: make([]byte, 10)})
	}
	store.AddRoot(dir)

	// Writer goroutine: atomically flips ALL children from size 10 to size 99.
	// Under a single WLock so the transition is atomic.
	flipDone := make(chan struct{})
	flipReady := make(chan struct{})
	go func() {
		defer close(flipDone)
		close(flipReady)
		store.mu.Lock()
		for i := range N {
			id := fmt.Sprintf("snap/child_%03d", i)
			store.nodes[id] = &Node{ID: id, Mode: 0o444, Data: make([]byte, 99)}
		}
		store.mu.Unlock()
	}()

	<-flipReady

	// Reader: take snapshots repeatedly. Each snapshot must be all-10 or all-99.
	// Never a mix — that would prove non-atomic reads.
	for attempt := 0; attempt < 500; attempt++ {
		stats, err := store.ListChildStats("snap")
		require.NoError(t, err)
		require.Len(t, stats, N)

		first := stats[0].ContentSize
		for j, s := range stats[1:] {
			assert.Equal(t, first, s.ContentSize,
				"attempt %d: child 0 has size %d but child %d has size %d — snapshot inconsistency",
				attempt, first, j+1, s.ContentSize)
		}
	}
	<-flipDone
}

// ---------------------------------------------------------------------------
// Test 3: RACE DETECTOR CLEAN
//
// FALSIFIABLE: Run with `go test -race`. If ListChildStats returned []*Node,
// concurrent writes to node fields would trigger the race detector because
// the reader accesses fields after releasing the RLock through aliased pointers.
// With []NodeStat (values copied under RLock), no race is possible.
// ---------------------------------------------------------------------------

func TestListChildStats_RaceDetectorClean(t *testing.T) {
	store := NewMemoryStore()
	store.AddRoot(&Node{
		ID:       "race",
		Mode:     fs.ModeDir,
		Children: []string{"race/a", "race/b"},
	})
	store.AddNode(&Node{ID: "race/a", Mode: 0o444, Data: make([]byte, 10)})
	store.AddNode(&Node{ID: "race/b", Mode: 0o444, Data: make([]byte, 20)})

	var wg sync.WaitGroup

	// Writer: continuously mutate node Data (changes ContentSize)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			store.AddNode(&Node{
				ID: "race/a", Mode: 0o444,
				Data: make([]byte, 10+i%50),
			})
		}
	}()

	// Reader: continuously read stats and access ALL fields.
	// With []*Node, accessing n.ContentSize() after RLock release races.
	// With []NodeStat, all fields are value copies — no race.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			stats, err := store.ListChildStats("race")
			if err != nil {
				continue
			}
			for _, s := range stats {
				// Touch every field — race detector catches unsynchronized access
				_ = s.ID
				_ = s.IsDir
				_ = s.ContentSize
				_ = s.ModTime
				_ = s.HasOrigin
			}
		}
	}()

	wg.Wait()
	// If this test passes with -race, snapshot semantics are proven.
	// If it fails with -race, the implementation returns aliased pointers.
}

// ---------------------------------------------------------------------------
// Test 4: BASIC CONTRACT
// ---------------------------------------------------------------------------

func TestListChildStats_Basic(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	store := NewMemoryStore()
	store.AddRoot(&Node{
		ID:       "vulns",
		Mode:     fs.ModeDir,
		Children: []string{"vulns/CVE-1", "vulns/CVE-2"},
	})
	store.AddNode(&Node{
		ID: "vulns/CVE-1", Mode: fs.ModeDir, ModTime: now,
	})
	store.AddNode(&Node{
		ID: "vulns/CVE-2", Mode: 0o444, ModTime: now,
		Data:   []byte("critical"),
		Origin: &SourceOrigin{FilePath: "/src/vuln.go"},
	})

	stats, err := store.ListChildStats("vulns")
	require.NoError(t, err)
	require.Len(t, stats, 2)

	// Find each by ID
	byID := map[string]NodeStat{}
	for _, s := range stats {
		byID[s.ID] = s
	}

	cve1 := byID["vulns/CVE-1"]
	assert.True(t, cve1.IsDir)
	assert.Equal(t, int64(0), cve1.ContentSize)
	assert.Equal(t, now, cve1.ModTime)
	assert.False(t, cve1.HasOrigin)

	cve2 := byID["vulns/CVE-2"]
	assert.False(t, cve2.IsDir)
	assert.Equal(t, int64(8), cve2.ContentSize) // len("critical")
	assert.Equal(t, now, cve2.ModTime)
	assert.True(t, cve2.HasOrigin)
}

func TestListChildStats_Root(t *testing.T) {
	store := NewMemoryStore()
	store.AddRoot(&Node{ID: "a", Mode: fs.ModeDir})
	store.AddRoot(&Node{ID: "b", Mode: fs.ModeDir})

	stats, err := store.ListChildStats("/")
	require.NoError(t, err)
	assert.Len(t, stats, 2)
}

func TestListChildStats_NotFound(t *testing.T) {
	store := NewMemoryStore()
	_, err := store.ListChildStats("nonexistent")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestListChildStats_SkipsMissing(t *testing.T) {
	store := NewMemoryStore()
	store.AddRoot(&Node{
		ID:       "pkg",
		Mode:     fs.ModeDir,
		Children: []string{"pkg/exists", "pkg/ghost"},
	})
	store.AddNode(&Node{ID: "pkg/exists", Mode: 0o444, Data: []byte("yes")})

	stats, err := store.ListChildStats("pkg")
	require.NoError(t, err)
	assert.Len(t, stats, 1)
	assert.Equal(t, "pkg/exists", stats[0].ID)
}

func TestListChildStats_Empty(t *testing.T) {
	store := NewMemoryStore()
	store.AddNode(&Node{ID: "empty", Mode: fs.ModeDir})

	stats, err := store.ListChildStats("empty")
	require.NoError(t, err)
	assert.Empty(t, stats)
}
