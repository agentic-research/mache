package cmd

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/agentic-research/mache/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stalenessGraph wraps a Graph with a scripted IndexStaleness answer, standing
// in for SQLiteGraph (the real reporter) without needing a .db fixture.
type stalenessGraph struct {
	graph.Graph
	rep graph.IndexStaleness
	ok  bool
}

func (s *stalenessGraph) IndexStaleness() (graph.IndexStaleness, bool) { return s.rep, s.ok }

// TestGetOverview_SurfacesIndexStaleness is the point of mache-6c9e1d's
// surfacing half: the default serve path is a frozen snapshot, and get_overview
// — the tool whose own description says "START HERE" — is where an agent must
// learn its answers may be stale. The block appears iff the graph can answer,
// and the human-readable warning appears iff there is actual drift.
func TestGetOverview_SurfacesIndexStaleness(t *testing.T) {
	built := time.Now().Add(-2 * time.Hour).Truncate(time.Second)

	overviewFor := func(t *testing.T, g graph.Graph) map[string]any {
		t.Helper()
		result, err := makeGetOverviewHandler(g)(context.Background(), makeRequest(nil))
		require.NoError(t, err)
		require.False(t, result.IsError)
		var ov map[string]any
		require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &ov))
		return ov
	}

	t.Run("drift reports the block and a warning", func(t *testing.T) {
		ov := overviewFor(t, &stalenessGraph{
			Graph: buildTestGraph(t),
			rep:   graph.IndexStaleness{BuiltAt: built, SourceRoot: "/src", ModifiedSince: 3},
			ok:    true,
		})
		idx, ok := ov["index"].(map[string]any)
		require.True(t, ok, "index block must be present when the graph can answer")
		assert.Equal(t, float64(3), idx["modified_since"])
		assert.Equal(t, "/src", idx["source_root"])
		warning, _ := ov["index_warning"].(string)
		assert.Contains(t, warning, "3 source file(s) modified since")
		assert.Contains(t, warning, built.Format("2006-01-02 15:04:05"))
	})

	t.Run("capped drift says the number is a floor", func(t *testing.T) {
		ov := overviewFor(t, &stalenessGraph{
			Graph: buildTestGraph(t),
			rep:   graph.IndexStaleness{BuiltAt: built, SourceRoot: "/src", ModifiedSince: 500, Capped: true},
			ok:    true,
		})
		warning, _ := ov["index_warning"].(string)
		assert.Contains(t, warning, "500+",
			"a capped count must read as a floor, not an exact number")
	})

	t.Run("fresh index reports the block but no warning", func(t *testing.T) {
		ov := overviewFor(t, &stalenessGraph{
			Graph: buildTestGraph(t),
			rep:   graph.IndexStaleness{BuiltAt: built, SourceRoot: "/src", ModifiedSince: 0},
			ok:    true,
		})
		_, hasIdx := ov["index"]
		assert.True(t, hasIdx, "a fresh answer is still an answer")
		assert.NotContains(t, ov, "index_warning", "zero drift must not cry wolf")
	})

	t.Run("unknown omits both — unknown is not fresh", func(t *testing.T) {
		ov := overviewFor(t, &stalenessGraph{Graph: buildTestGraph(t), ok: false})
		assert.NotContains(t, ov, "index")
		assert.NotContains(t, ov, "index_warning")
	})

	t.Run("non-reporting graph omits both", func(t *testing.T) {
		// MemoryStore does not implement StalenessReporter at all — the
		// handler must not invent an answer for it.
		ov := overviewFor(t, buildTestGraph(t))
		assert.NotContains(t, ov, "index")
		assert.NotContains(t, ov, "index_warning")
	})
}

// TestLazyGraph_ForwardsIndexStaleness pins the delegation that makes the
// feature real on the primary serve path: handlers hold *lazyGraph, and a
// capability the wrapper doesn't forward is a capability the daemon doesn't
// have. Found live — get_overview returned index:null against a daemon whose
// inner graph answered fine.
func TestLazyGraph_ForwardsIndexStaleness(t *testing.T) {
	want := graph.IndexStaleness{SourceRoot: "/src", ModifiedSince: 7}
	lg := &lazyGraph{inner: &stalenessGraph{Graph: buildTestGraph(t), rep: want, ok: true}}
	lg.once.Do(func() {}) // consume init: serve a pre-set inner, don't build one

	got, ok := lg.IndexStaleness()
	require.True(t, ok)
	assert.Equal(t, want, got)

	// A non-reporting inner graph stays unknown — the wrapper must not
	// answer on its own authority.
	plain := &lazyGraph{inner: buildTestGraph(t)}
	plain.once.Do(func() {})
	_, ok = plain.IndexStaleness()
	assert.False(t, ok)
}
