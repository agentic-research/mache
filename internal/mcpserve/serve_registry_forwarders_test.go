package mcpserve

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/fixturedb"
)

// countingStore counts its reference tokens as 42, which its refs map does not
// have, so an answer of 42 can only have come from RefTokenCount.
type countingStore struct{ *graph.MemoryStore }

func (countingStore) RefTokenCount() (int, error) { return 42, nil }

// get_overview, given the lazyGraph every serve-mode session holds, counts
// through the backend's RefTokenCount. Without the forwarder it took the
// fallback, building the whole refs map: 1.1 s on mache's own repo through the
// daemon against 0.24 s in process (mache-a6c283).
func TestGetOverview_ThroughLazyGraph_CountsWithTheBackend(t *testing.T) {
	s := graph.NewMemoryStore()
	require.NoError(t, s.AddRef("Handler", "pkg/functions/Serve"))
	res, err := makeGetOverviewHandler(newTestLazyGraph(countingStore{s}, ""))(context.Background(), mcp.CallToolRequest{})
	require.NoError(t, err)
	var ov struct {
		RefTokens int `json:"ref_tokens"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcp.TextContent).Text), &ov))
	assert.Equal(t, 42, ov.RefTokens)
}

// Each forwarder answers as the SQLiteGraph it wraps.
func TestLazyGraph_ForwardersAnswerAsTheBackend(t *testing.T) {
	path, _ := fixturedb.New(t, fixturedb.Leyline).
		Def("Serve", "pkg/a.go/function_declaration_0", "function").
		Ref("Serve", "pkg/b.go/function_declaration_0", "", "").
		Build()
	sg, err := graph.OpenSQLiteGraph(path, &api.Topology{}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sg.Close() })
	lg := newTestLazyGraph(sg, "")

	want, err := sg.RefTokenCount()
	require.NoError(t, err)
	got, err := lg.RefTokenCount()
	require.NoError(t, err)
	assert.Equal(t, want, got)

	wantDefs, err := sg.LookupDefNodes("Serve")
	require.NoError(t, err)
	require.NotEmpty(t, wantDefs)
	gotDefs, err := lg.LookupDefNodes("Serve")
	require.NoError(t, err)
	assert.Equal(t, wantDefs, gotDefs)

	refID := sg.RefsMap()["Serve"][0]
	wantRange, err := sg.RefRangeOf(refID)
	require.NoError(t, err)
	gotRange, err := lg.RefRangeOf(refID)
	require.NoError(t, err)
	assert.Equal(t, wantRange, gotRange)

	wantTarget, err := sg.ResolveRef(refID)
	require.NoError(t, err)
	gotTarget, err := lg.ResolveRef(refID)
	require.NoError(t, err)
	assert.Equal(t, wantTarget, gotTarget)
}

// Over a backend without the capability each forwarder answers as the
// interface documents for "not available".
func TestLazyGraph_ForwardersWithoutTheCapability(t *testing.T) {
	s := graph.NewMemoryStore()
	require.NoError(t, s.AddRef("Handler", "pkg/functions/Serve"))
	require.NoError(t, s.AddRef("Marshal", "pkg/functions/Encode"))
	lg := newTestLazyGraph(s, "")

	n, err := lg.RefTokenCount()
	require.NoError(t, err)
	assert.Equal(t, len(s.RefsMap()), n, "counted as the refs map's length")

	nodes, err := lg.LookupDefNodes("Handler")
	require.NoError(t, err)
	assert.Nil(t, nodes)

	r, err := lg.RefRangeOf("pkg/functions/Serve")
	require.NoError(t, err)
	assert.Nil(t, r, "no spans")

	_, err = lg.ResolveRef("pkg/functions/Serve")
	assert.Error(t, err, "cannot classify without a SQLite-backed graph")
}
