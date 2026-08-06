package cmd

import (
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pathfulStore is a graph.Graph that also knows its own .db path — the shape
// SQLiteGraph and WritableGraph both have in production. MemoryStore supplies
// Graph and QueryRefs; only DBPath is added.
type pathfulStore struct {
	*graph.MemoryStore
	dbPath string
}

func (s *pathfulStore) DBPath() string { return s.dbPath }

// pathlessStore is the other production shape: a backend with no file behind
// it at all.
type pathlessStore struct{ *graph.MemoryStore }

func TestLazyGraph_ForwardsDBPathToInner(t *testing.T) {
	want := filepath.Join(t.TempDir(), "project.db")
	lg := newTestLazyGraph(&pathfulStore{MemoryStore: graph.NewMemoryStore(), dbPath: want}, "")

	assert.Equal(t, want, lg.DBPath(),
		"handlers hold the lazyGraph, not the backend — an unforwarded DBPath makes every capnp readthrough a no-op")
}

func TestLazyGraph_DBPathEmptyWhenBackendHasNone(t *testing.T) {
	lg := newTestLazyGraph(&pathlessStore{MemoryStore: graph.NewMemoryStore()}, "")

	assert.Empty(t, lg.DBPath(),
		`"" is the documented no-source sentinel; readLSPRefsFromCapnp and LoadCapnpBindings both no-op on it`)
}

// TestQueryLSPRefs_ThroughLazyGraph_ReadsSiblingCapnpLog is the regression.
// It exercises the real production call — queryLSPRefs against the graph a
// handler actually holds — rather than against the backend directly, which is
// what hid the defect: every existing queryLSPRefs test passes a querier that
// already implements dbPathProvider, so the capnp path was well covered and
// the serve-mode path that reaches it was not covered at all.
//
// The fixture deliberately has NO _lsp_refs table, so the legacy SQL fallback
// can contribute nothing. Any result here must have come from the capnp log.
func TestQueryLSPRefs_ThroughLazyGraph_ReadsSiblingCapnpLog(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "project.db")
	writeBindingLogForTest(t, dbPath,
		"pkg/functions/Read", "Read", "pkg/functions/Caller", "file:///pkg/caller.go")

	lg := newTestLazyGraph(&pathfulStore{MemoryStore: graph.NewMemoryStore(), dbPath: dbPath}, "")

	refs, err := queryLSPRefs(lg, "Read")
	require.NoError(t, err)
	require.Len(t, refs, 1,
		"the sibling .bindings.capnp log must be reachable through the lazyGraph; "+
			"without the DBPath forwarder this falls through to the retired _lsp_refs SQL path and returns nothing")
	assert.Equal(t, "pkg/functions/Read", refs[0].NodeID)
	assert.Equal(t, "file:///pkg/caller.go", refs[0].URI)
}

// TestQueryLSPRefs_ThroughLazyGraph_NoLogFallsBackCleanly is the negative: the
// forwarder must not turn "no capnp log" into an error. A missing log has to
// stay a clean fall-through to the legacy path.
func TestQueryLSPRefs_ThroughLazyGraph_NoLogFallsBackCleanly(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "project.db")
	store := graph.NewMemoryStore()
	// The legacy path actually issues SQL, so this fixture needs the refs DB a
	// real backend always has. The positive test above deliberately does not
	// initialize it — proof that it never reaches the fallback, and so that
	// its result can only have come from the capnp log.
	require.NoError(t, store.InitRefsDB())
	lg := newTestLazyGraph(&pathfulStore{MemoryStore: store, dbPath: dbPath}, "")

	refs, err := queryLSPRefs(lg, "Read")
	require.NoError(t, err, "a missing sibling log is not an error condition")
	assert.Empty(t, refs)
}
