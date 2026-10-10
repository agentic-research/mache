package mcpserve

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/agentic-research/mache/graph"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// get_architecture ranks symbols by fan-in and by definition count, then keeps
// the top 20. Both inputs come from Go maps, whose iteration order is
// randomized, so without a total order the TIES came out in a different order
// on every call, and a tie straddling the cut decided which symbols appeared at
// all. The release parity run (tools/release-parity) caught it: the same
// binary answered the same call two different ways.
//
// 30 tokens that all tie, cut to 20, makes a lucky pass vanishingly unlikely
// under the old code: every call would have to draw the same 20 of 30 in the
// same order.
func TestGetArchitecture_TiesRankDeterministically(t *testing.T) {
	store := graph.NewMemoryStore()
	for i := range 30 {
		sym := fmt.Sprintf("sym%02d", i)
		require.NoError(t, store.AddRef(sym, "pkg/f.go/caller"))
		require.NoError(t, store.AddDef(sym, "pkg/f.go/"+sym))
	}
	handler := makeGetArchitectureHandler(store)

	type ranked struct {
		MostReferenced []struct {
			Symbol string `json:"symbol"`
		} `json:"most_referenced"`
		KeyAbstractions []struct {
			Symbol string `json:"symbol"`
		} `json:"key_abstractions"`
	}
	call := func() (string, ranked) {
		res, err := handler(context.Background(), mcp.CallToolRequest{})
		require.NoError(t, err)
		require.False(t, res.IsError)
		text := res.Content[0].(mcp.TextContent).Text
		var r ranked
		require.NoError(t, json.Unmarshal([]byte(text), &r))
		return text, r
	}

	first, r := call()
	for range 20 {
		again, _ := call()
		require.Equal(t, first, again, "get_architecture answered the same call differently")
	}

	// The tie-break is the symbol name, so the kept 20 are the first 20 names.
	want := make([]string, 20)
	for i := range want {
		want[i] = fmt.Sprintf("sym%02d", i)
	}
	var gotRefs, gotDefs []string
	for _, e := range r.MostReferenced {
		gotRefs = append(gotRefs, e.Symbol)
	}
	for _, e := range r.KeyAbstractions {
		gotDefs = append(gotDefs, e.Symbol)
	}
	assert.Equal(t, want, gotRefs, "most_referenced ties must break on symbol name")
	assert.Equal(t, want, gotDefs, "key_abstractions ties must break on symbol name")
}
