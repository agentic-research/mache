package graph

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hubbedRefs builds two genuinely separate clusters plus one hub token that
// every node references — the shape of `t`, `err`, or `len` in a real Go
// codebase.
func hubbedRefs(clusterSize int) map[string][]string {
	refs := map[string][]string{}
	var all []string
	for _, side := range []string{"a", "b"} {
		var members []string
		for i := range clusterSize {
			members = append(members, fmt.Sprintf("%s/node%d", side, i))
		}
		// Tokens shared only within the cluster: the real signal.
		for i := range 3 {
			refs[fmt.Sprintf("%s_local%d", side, i)] = members
		}
		all = append(all, members...)
	}
	refs["t"] = all // the hub: referenced by everything, means nothing
	return refs
}

// TestPruneHubTokens_RemovesTheHubAndSaysSo covers why pruning exists at all.
//
// The projection connects every PAIR of nodes sharing a token, so a token with
// fan-in K costs K(K-1)/2 edges. Measured on mache itself: `t` (the *testing.T
// receiver, K=9,903) alone accounted for 49M of 84.8M edge insertions — 58% of
// the total, for a token that tells you nothing about what code belongs
// together.
func TestPruneHubTokens_RemovesTheHubAndSaysSo(t *testing.T) {
	refs := hubbedRefs(40) // hub fan-in 80, locals 40

	kept, pruned := pruneHubTokens(refs, 50)

	require.Len(t, pruned, 1, "only the hub exceeds the cap")
	assert.Equal(t, "t", pruned[0].Token)
	assert.Equal(t, 80, pruned[0].FanIn, "must report the fan-in that got it pruned")
	assert.NotContains(t, kept, "t")
	assert.Len(t, kept, 6, "every below-cap token survives")
}

// TestPruneHubTokens_CapIsInclusive pins the boundary. maxFanIn is the largest
// fan-in a token may have and still COUNT — "above which a token is treated as
// a hub" — so a token sitting exactly on it survives.
//
// Without a token exactly at the cap the boundary is unobservable: a `>` to
// `>=` mutation survived the rest of this file, because every fixture token was
// comfortably above or below.
func TestPruneHubTokens_CapIsInclusive(t *testing.T) {
	refs := map[string][]string{
		"exactly_at": make([]string, 50),
		"one_over":   make([]string, 51),
		"one_under":  make([]string, 49),
	}
	for k, v := range refs {
		for i := range v {
			v[i] = fmt.Sprintf("%s/n%d", k, i)
		}
	}

	kept, pruned := pruneHubTokens(refs, 50)

	require.Len(t, pruned, 1, "only the token ABOVE the cap is a hub")
	assert.Equal(t, "one_over", pruned[0].Token)
	assert.Contains(t, kept, "exactly_at", "a token exactly at the cap is kept")
	assert.Contains(t, kept, "one_under")
}

// TestPruneHubTokens_DisabledAndNoopCasesShareTheInput keeps the common path
// allocation-free: nothing to prune means the caller's map is handed straight
// back rather than copied.
func TestPruneHubTokens_DisabledAndNoopCasesShareTheInput(t *testing.T) {
	refs := hubbedRefs(5)

	for _, tc := range []struct {
		name string
		cap  int
	}{
		{"pruning disabled", 0},
		{"negative disables", -1},
		{"cap above every fan-in", 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kept, pruned := pruneHubTokens(refs, tc.cap)
			assert.Empty(t, pruned)
			assert.Len(t, kept, len(refs), "nothing removed")
		})
	}
}

// TestDetectCommunities_HubDestroysThePartitionUntilPruned is the load-bearing
// one: pruning is not merely an optimisation, it is what makes the ANSWER
// correct.
//
// A hub joins every node into a near-complete subgraph, so Louvain collapses
// two genuinely separate clusters into one. Measured on mache: modularity rose
// from 0.543 to 0.985 and the run went from 32.5s to 215ms once hubs were
// dropped — faster AND a better partition, from the same change.
func TestDetectCommunities_HubDestroysThePartitionUntilPruned(t *testing.T) {
	refs := hubbedRefs(30) // hub fan-in 60

	withHub := DetectCommunitiesWithFanIn(refs, 2, 0) // pruning off
	pruned := DetectCommunitiesWithFanIn(refs, 2, 50) // hub removed

	assert.Greater(t, pruned.Modularity, withHub.Modularity,
		"dropping a token that joins everything must IMPROVE separation, not just speed")
	assert.GreaterOrEqual(t, len(pruned.Communities), 2,
		"the two clusters are separable once the hub is gone")
	assert.Len(t, pruned.PrunedTokens, 1,
		"and the caller is told which token stopped shaping the result")
	assert.Empty(t, withHub.PrunedTokens, "cap 0 disables pruning entirely")
}

// TestDetectCommunities_DefaultPrunes pins that the DEFAULT entry point prunes.
// Every existing caller (get_architecture, get_communities, get_diagram, pack,
// engine_diagram) goes through it, and they were all silently getting
// hub-dominated partitions.
func TestDetectCommunities_DefaultPrunes(t *testing.T) {
	refs := hubbedRefs(DefaultMaxFanIn) // hub fan-in 2*DefaultMaxFanIn

	got := DetectCommunities(refs, 2)

	require.Len(t, got.PrunedTokens, 1, "the default must prune, not just the explicit variant")
	assert.Equal(t, "t", got.PrunedTokens[0].Token)
}
