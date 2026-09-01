package mcpserve

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/testutil"
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
// already implements graph.DBPathProvider, so the capnp path was well covered and
// the serve-mode path that reaches it was not covered at all.
//
// The fixture deliberately has NO _lsp_refs table, so the legacy SQL fallback
// can contribute nothing. Any result here must have come from the capnp log.
func TestQueryLSPRefs_ThroughLazyGraph_ReadsSiblingCapnpLog(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "project.db")
	testutil.WriteBindingLogForTest(t, dbPath,
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

// TestFindCallers_ReadsCapnpRefsThroughLazyGraph closes the gap the smell side
// already had covered and the caller side did not.
//
// find_callers is the OTHER consumer of the missing DBPath forwarder, but its
// existing coverage misses the production configuration twice over:
// TestFindCallers_WithLSPRefs hands the backend straight to
// makeFindCallersHandler, never through a lazyGraph, and it exercises the
// legacy `_lsp_refs` SQL rows rather than the sibling capnp log that replaced
// them. So handler -> lazyGraph -> capnp — exactly what serve mode does on any
// .db built after LLO T8.2 — had no test at all.
//
// The fixture's MemoryStore has no refs DB initialized, which makes the
// assertion self-enforcing: if the capnp path is NOT taken, queryLSPRefs falls
// through to the legacy SQL path, that errors, the handler drops the lsp_refs
// block entirely, and this fails.
func TestFindCallers_ReadsCapnpRefsThroughLazyGraph(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "project.db")
	testutil.WriteBindingLogForTest(t, dbPath,
		"pkg/functions/Validate", "Validate", "pkg/functions/Caller", "file:///pkg/caller.go")

	lg := newTestLazyGraph(&pathfulStore{MemoryStore: graph.NewMemoryStore(), dbPath: dbPath}, "")

	res, err := makeFindCallersHandler(lg)(context.Background(), testutil.MakeRequest(map[string]any{
		"token": "Validate",
	}))
	require.NoError(t, err)
	require.False(t, res.IsError, "handler must succeed: %s", testutil.ResultText(t, res))

	var out struct {
		Callers []string         `json:"callers"`
		LSPRefs []lspRefLocation `json:"lsp_refs"`
	}
	require.NoError(t, json.Unmarshal([]byte(testutil.ResultText(t, res)), &out))
	require.Len(t, out.LSPRefs, 1,
		"find_callers must supplement from the sibling capnp log through the lazyGraph; "+
			"without the DBPath forwarder this block is absent entirely")
	assert.Equal(t, "file:///pkg/caller.go", out.LSPRefs[0].URI)
	assert.Equal(t, "pkg/functions/Validate", out.LSPRefs[0].NodeID)
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
