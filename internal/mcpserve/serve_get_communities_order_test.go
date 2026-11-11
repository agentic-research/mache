package mcpserve

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/testutil"
)

// TestGetCommunities_EqualSizedCommunitiesKeepTheirOrder pins the order the
// full response shows communities in: the total order DetectCommunities
// returns (size, then smallest member), with IDs ascending in step. The
// handler used to re-sort that output by size alone with an unstable sort
// (mache-82ded3); that happened to be a no-op only because Go's sort leaves an
// input its comparator already considers sorted untouched, a fact nothing
// here should lean on. The graph has more communities than the truncation
// cap so the order is pinned through truncation as well.
func TestGetCommunities_EqualSizedCommunitiesKeepTheirOrder(t *testing.T) {
	store := graph.NewMemoryStore()
	for i := range 40 {
		token := fmt.Sprintf("tok%02d", i)
		require.NoError(t, store.AddRef(token, fmt.Sprintf("pkg/f%02d.go/a", i)))
		require.NoError(t, store.AddRef(token, fmt.Sprintf("pkg/f%02d.go/b", i)))
	}
	handler := makeGetCommunitiesHandler(store)

	for range 5 {
		result, err := handler(context.Background(), testutil.MakeRequest(nil))
		require.NoError(t, err)
		require.False(t, result.IsError)

		var cr graph.CommunityResult
		require.NoError(t, json.Unmarshal([]byte(testutil.ResultText(t, result)), &cr))
		require.Len(t, cr.Communities, 25, "truncated to the cap")
		for i, c := range cr.Communities {
			assert.Equal(t, i, c.ID, "IDs ascend in the order shown")
			assert.Equal(t, fmt.Sprintf("pkg/f%02d.go/a", i), c.Members[0],
				"equal sizes order on the smallest member")
		}
	}
}
