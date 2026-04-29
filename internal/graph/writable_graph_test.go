package graph

import (
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// Bead mache-02f774 — WritableGraph's write path (UpdateRecord, Invalidate,
// Flush) had zero direct test coverage. The graph suite exercised the
// shared read methods only; the writes were covered transitively (or not
// at all) by higher-level integration tests. These tests pin down the
// behaviour so the splice pipeline can rely on it.

func newTestWritableGraph(t *testing.T) *WritableGraph {
	t.Helper()
	dbPath := createNodesTableDB(t)
	schema := &api.Topology{Table: "results"}
	g, err := OpenWritableGraph(dbPath, schema, stubRender, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })
	return g
}

func TestWritableGraph_UpdateRecord_RewritesContent(t *testing.T) {
	g := newTestWritableGraph(t)

	const newContent = "func Validate() bool { return true }"
	require.NoError(t, g.UpdateRecord("pkg/auth/source", []byte(newContent)))

	// Read it back — content reflects the update, size matches.
	buf := make([]byte, len(newContent)+8)
	n, err := g.ReadContent("pkg/auth/source", buf, 0)
	require.NoError(t, err)
	assert.Equal(t, newContent, string(buf[:n]))

	stats, err := g.ListChildStats("pkg/auth")
	require.NoError(t, err)
	for _, s := range stats {
		if filepath.Base(s.ID) == "source" {
			assert.Equal(t, int64(len(newContent)), s.ContentSize,
				"size cache must reflect the new content length")
			return
		}
	}
	t.Fatal("source child not found after update")
}

func TestWritableGraph_UpdateRecord_BumpsModTime(t *testing.T) {
	g := newTestWritableGraph(t)

	pre, err := g.GetNode("pkg/auth/source")
	require.NoError(t, err)
	preMtime := pre.ModTime

	require.NoError(t, g.UpdateRecord("pkg/auth/source", []byte("changed")))

	post, err := g.GetNode("pkg/auth/source")
	require.NoError(t, err)
	assert.True(t, post.ModTime.After(preMtime),
		"UpdateRecord must bump ModTime forward (was %v, now %v)", preMtime, post.ModTime)
}

func TestWritableGraph_UpdateRecord_NormalizesLeadingSlash(t *testing.T) {
	g := newTestWritableGraph(t)

	require.NoError(t, g.UpdateRecord("/pkg/auth/source", []byte("normalized")))

	buf := make([]byte, 32)
	n, err := g.ReadContent("pkg/auth/source", buf, 0)
	require.NoError(t, err)
	assert.Equal(t, "normalized", string(buf[:n]))
}

func TestWritableGraph_UpdateRecord_MissingNodeReturnsErrNotFound(t *testing.T) {
	g := newTestWritableGraph(t)

	err := g.UpdateRecord("does/not/exist", []byte("x"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound),
		"missing target must wrap ErrNotFound; got %v", err)
}

// TestWritableGraph_UpdateRecord_TwoStepShrink covers the write→shrink
// roundtrip: replace content with a non-empty value first, then shrink
// to a single byte. Pins down that size + content stay consistent across
// multiple writes (shrinking previously left stale size cache lingering
// on at least one backend during development).
func TestWritableGraph_UpdateRecord_TwoStepShrink(t *testing.T) {
	g := newTestWritableGraph(t)

	require.NoError(t, g.UpdateRecord("pkg/auth/source", []byte("longer-content-than-original")))
	require.NoError(t, g.UpdateRecord("pkg/auth/source", []byte("x")))

	buf := make([]byte, 8)
	n, err := g.ReadContent("pkg/auth/source", buf, 0)
	require.NoError(t, err)
	assert.Equal(t, "x", string(buf[:n]))

	node, err := g.GetNode("pkg/auth/source")
	require.NoError(t, err)
	assert.Equal(t, int64(1), node.ContentSize())
}

// TestWritableGraph_UpdateRecord_InvalidatesReadCache catches the regression
// where a stale ReadContent / ListChildStats result lingers after an
// UpdateRecord. The reader's cache is keyed by id, so we must see the new
// bytes on the very next call.
func TestWritableGraph_UpdateRecord_InvalidatesReadCache(t *testing.T) {
	g := newTestWritableGraph(t)

	// Prime the read cache with the original content.
	original := make([]byte, 64)
	n, err := g.ReadContent("pkg/auth/source", original, 0)
	require.NoError(t, err)
	require.Greater(t, n, 0)

	const updated = "wholly new content goes here"
	require.NoError(t, g.UpdateRecord("pkg/auth/source", []byte(updated)))

	buf := make([]byte, 64)
	n, err = g.ReadContent("pkg/auth/source", buf, 0)
	require.NoError(t, err)
	assert.Equal(t, updated, string(buf[:n]),
		"read after update returned stale cached content — Invalidate not propagated")
}

// TestWritableGraph_UpdateRecord_ConcurrentSerialised pins down that
// concurrent UpdateRecord calls don't leave the row in a torn state.
// The mutex inside WritableGraph serialises writes; readers see one of
// the inputs verbatim, never a partial mix.
func TestWritableGraph_UpdateRecord_ConcurrentSerialised(t *testing.T) {
	g := newTestWritableGraph(t)

	const a = "AAAAAAAAAAAAAAAA"
	const b = "BBBBBBBBBBBBBBBB"

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = g.UpdateRecord("pkg/auth/source", []byte(a))
		}()
		go func() {
			defer wg.Done()
			_ = g.UpdateRecord("pkg/auth/source", []byte(b))
		}()
	}
	wg.Wait()

	buf := make([]byte, 32)
	n, err := g.ReadContent("pkg/auth/source", buf, 0)
	require.NoError(t, err)
	got := string(buf[:n])
	assert.True(t, got == a || got == b,
		"final content must be one of the inputs verbatim, got %q", got)
}

// TestWritableGraph_FlushNoFlusher exercises the non-flusher branch.
// The mount path can pass a nil flusher when arena writeback isn't wired,
// and Flush/FlushNow must not panic.
func TestWritableGraph_FlushNoFlusher(t *testing.T) {
	g := newTestWritableGraph(t)
	g.Flush() // no panic
	require.NoError(t, g.FlushNow())
}

// TestWritableGraph_Invalidate_ClearsSizeCache verifies Invalidate
// reaches through to the NodesTableReader's caches so a subsequent
// ListChildStats picks up an out-of-band change.
func TestWritableGraph_Invalidate_ClearsSizeCache(t *testing.T) {
	g := newTestWritableGraph(t)

	// Warm size cache.
	_, err := g.ListChildStats("pkg/auth")
	require.NoError(t, err)

	// Bypass UpdateRecord and rewrite directly so Invalidate is the only
	// thing keeping the reader honest.
	const fresh = "this came from outside the WritableGraph"
	_, err = g.ntr.DB().Exec(
		"UPDATE nodes SET record = ?, size = ? WHERE id = ?",
		fresh, len(fresh), "pkg/auth/source",
	)
	require.NoError(t, err)

	g.Invalidate("pkg/auth/source")

	stats, err := g.ListChildStats("pkg/auth")
	require.NoError(t, err)
	for _, s := range stats {
		if filepath.Base(s.ID) == "source" {
			assert.Equal(t, int64(len(fresh)), s.ContentSize,
				"after Invalidate, ListChildStats must report the post-write size")
			return
		}
	}
	t.Fatal("source child missing")
}

// TestWritableGraph_DBPath returns the path passed to OpenWritableGraph.
func TestWritableGraph_DBPath(t *testing.T) {
	dbPath := createNodesTableDB(t)
	schema := &api.Topology{Table: "results"}
	g, err := OpenWritableGraph(dbPath, schema, stubRender, nil)
	require.NoError(t, err)
	defer func() { _ = g.Close() }()

	assert.Equal(t, dbPath, g.DBPath())
}

// TestOpenWritableGraph_RequiresNodesTable rejects opening a DB that has
// no nodes table — protects against silently mounting an unprojected DB.
func TestOpenWritableGraph_RequiresNodesTable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "no-nodes.db")
	bare, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	_, err = bare.Exec(`CREATE TABLE results (id TEXT PRIMARY KEY, record JSON)`)
	require.NoError(t, err)
	require.NoError(t, bare.Close())

	_, err = OpenWritableGraph(dbPath, &api.Topology{Table: "results"}, stubRender, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nodes table")
}
