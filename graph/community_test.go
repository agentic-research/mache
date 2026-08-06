package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// DetectCommunities tests
// ---------------------------------------------------------------------------

func TestDetectCommunities_TwoClusters(t *testing.T) {
	// Two groups that share tokens within-group but not across-group.
	// Cluster A: a1, a2, a3 share tokens "alpha", "beta"
	// Cluster B: b1, b2, b3 share tokens "gamma", "delta"
	refs := map[string][]string{
		"alpha": {"a1", "a2", "a3"},
		"beta":  {"a1", "a2", "a3"},
		"gamma": {"b1", "b2", "b3"},
		"delta": {"b1", "b2", "b3"},
	}

	result := DetectCommunities(refs, 2)

	require.NotNil(t, result)
	assert.Equal(t, 6, result.NumNodes)
	assert.Equal(t, 2, len(result.Communities), "should detect 2 communities")
	assert.Greater(t, result.Modularity, 0.0, "modularity should be positive for well-separated clusters")

	// Each community should have 3 members
	for _, c := range result.Communities {
		assert.Len(t, c.Members, 3)
	}

	// Verify membership consistency
	for _, c := range result.Communities {
		for _, m := range c.Members {
			assert.Equal(t, c.ID, result.Membership[m])
		}
	}
}

func TestDetectCommunities_SingleCluster(t *testing.T) {
	// All nodes share the same tokens → one community
	refs := map[string][]string{
		"shared1": {"a", "b", "c", "d"},
		"shared2": {"a", "b", "c", "d"},
	}

	result := DetectCommunities(refs, 2)
	require.NotNil(t, result)
	assert.Equal(t, 4, result.NumNodes)
	assert.Len(t, result.Communities, 1)
	assert.Len(t, result.Communities[0].Members, 4)
}

func TestDetectCommunities_Empty(t *testing.T) {
	result := DetectCommunities(nil, 0)
	require.NotNil(t, result)
	assert.Empty(t, result.Communities)
	assert.Equal(t, 0, result.NumNodes)
}

func TestDetectCommunities_SingletonTokens(t *testing.T) {
	// Each token referenced by only one node → no edges → no communities (min size 2)
	refs := map[string][]string{
		"t1": {"a"},
		"t2": {"b"},
		"t3": {"c"},
	}

	result := DetectCommunities(refs, 2)
	require.NotNil(t, result)
	assert.Empty(t, result.Communities, "singletons should be filtered out")
}

func TestDetectCommunities_MinCommunitySize(t *testing.T) {
	refs := map[string][]string{
		"shared": {"a", "b", "c", "d", "e"},
	}

	// With min size 3, should include the community
	result := DetectCommunities(refs, 3)
	assert.Len(t, result.Communities, 1)

	// With min size 10, should filter it out
	result = DetectCommunities(refs, 10)
	assert.Empty(t, result.Communities)
}

func TestDetectCommunities_WeakBridge(t *testing.T) {
	// Two clusters with a weak bridge node
	// Cluster A: a1, a2, a3 share "alpha" and "beta" (strong internal edges)
	// Cluster B: b1, b2, b3 share "gamma" and "delta" (strong internal edges)
	// Bridge: "bridge_token" connects a1 and b1 (weak cross-cluster edge)
	refs := map[string][]string{
		"alpha":        {"a1", "a2", "a3"},
		"beta":         {"a1", "a2", "a3"},
		"gamma":        {"b1", "b2", "b3"},
		"delta":        {"b1", "b2", "b3"},
		"bridge_token": {"a1", "b1"},
	}

	result := DetectCommunities(refs, 2)
	require.NotNil(t, result)
	assert.GreaterOrEqual(t, len(result.Communities), 2, "should detect at least 2 communities despite bridge")
	assert.Greater(t, result.Modularity, 0.0)
}

func TestDetectCommunities_SortedBySize(t *testing.T) {
	// Create communities of different sizes
	refs := map[string][]string{
		"small":  {"s1", "s2"},
		"medium": {"m1", "m2", "m3"},
		"large":  {"l1", "l2", "l3", "l4"},
	}

	result := DetectCommunities(refs, 2)
	require.NotNil(t, result)

	// Verify sorted by size descending
	for i := 1; i < len(result.Communities); i++ {
		assert.GreaterOrEqual(t,
			len(result.Communities[i-1].Members),
			len(result.Communities[i].Members),
			"communities should be sorted by size descending",
		)
	}
}

func TestDetectCommunities_MembershipMap(t *testing.T) {
	refs := map[string][]string{
		"t1": {"a", "b"},
		"t2": {"c", "d"},
	}

	result := DetectCommunities(refs, 2)
	require.NotNil(t, result)

	// Every member in a community should appear in the membership map
	for _, c := range result.Communities {
		for _, m := range c.Members {
			commID, ok := result.Membership[m]
			assert.True(t, ok, "member %s should be in membership map", m)
			assert.Equal(t, c.ID, commID)
		}
	}
}

func TestDetectCommunities_IDsAreSequential(t *testing.T) {
	refs := map[string][]string{
		"t1": {"a", "b", "c"},
		"t2": {"d", "e", "f"},
		"t3": {"g", "h", "i"},
	}

	result := DetectCommunities(refs, 2)
	for i, c := range result.Communities {
		assert.Equal(t, i, c.ID, "community IDs should be sequential starting from 0")
	}
}

func TestDetectCommunities_DuplicateNodesInRefs(t *testing.T) {
	// Same node appears multiple times under same token
	refs := map[string][]string{
		"t1": {"a", "a", "b"},
		"t2": {"b", "c"},
	}

	// Should not panic and should handle gracefully
	result := DetectCommunities(refs, 2)
	require.NotNil(t, result)
}

func TestDetectCommunities_LargerGraph(t *testing.T) {
	// Simulate a more realistic scenario: 3 packages with internal refs
	refs := map[string][]string{
		// Package "auth" functions call each other
		"ValidateToken": {"auth/handler", "auth/middleware", "auth/service"},
		"ParseJWT":      {"auth/handler", "auth/service"},
		"HashPassword":  {"auth/service", "auth/repository"},

		// Package "api" functions call each other
		"HandleRequest": {"api/router", "api/handler", "api/middleware"},
		"SerializeJSON": {"api/handler", "api/response"},
		"ParseQuery":    {"api/router", "api/handler"},

		// Package "db" functions call each other
		"OpenConn":  {"db/pool", "db/migrate", "db/repository"},
		"ExecQuery": {"db/repository", "db/pool"},
		"BeginTx":   {"db/repository", "db/migrate"},

		// Cross-package refs (weaker)
		"Logger": {"auth/service", "api/middleware", "db/pool"},
	}

	result := DetectCommunities(refs, 2)
	require.NotNil(t, result)

	// Should detect meaningful clusters
	assert.GreaterOrEqual(t, len(result.Communities), 2,
		"should detect multiple communities in a multi-package graph")
	assert.Greater(t, result.Modularity, 0.0,
		"modularity should be positive for structured code")

	// Total unique nodes: auth/{handler,middleware,service,repository} +
	// api/{router,handler,middleware,response} + db/{pool,migrate,repository} = 11
	assert.Equal(t, 11, result.NumNodes)
}

// ---------------------------------------------------------------------------
// ConnectedComponents tests
// ---------------------------------------------------------------------------

func TestConnectedComponents_TwoDisjoint(t *testing.T) {
	refs := map[string][]string{
		"t1": {"a", "b"},
		"t2": {"c", "d"},
	}

	components := ConnectedComponents(refs)
	assert.Len(t, components, 2)

	// Each component should have 2 members
	for _, comp := range components {
		assert.Len(t, comp, 2)
	}
}

func TestConnectedComponents_SingleComponent(t *testing.T) {
	refs := map[string][]string{
		"t1": {"a", "b"},
		"t2": {"b", "c"},
		"t3": {"c", "d"},
	}

	components := ConnectedComponents(refs)
	assert.Len(t, components, 1)
	assert.Len(t, components[0], 4)
}

func TestConnectedComponents_Empty(t *testing.T) {
	components := ConnectedComponents(nil)
	assert.Nil(t, components)
}

func TestConnectedComponents_Singletons(t *testing.T) {
	refs := map[string][]string{
		"t1": {"a"},
		"t2": {"b"},
	}

	components := ConnectedComponents(refs)
	// Each singleton is its own component
	assert.Len(t, components, 2)
	for _, comp := range components {
		assert.Len(t, comp, 1)
	}
}

func TestConnectedComponents_SortedBySize(t *testing.T) {
	refs := map[string][]string{
		"t1": {"a", "b", "c", "d"}, // large component
		"t2": {"e", "f"},           // small component
	}

	components := ConnectedComponents(refs)
	require.Len(t, components, 2)
	assert.Len(t, components[0], 4, "first should be the larger component")
	assert.Len(t, components[1], 2, "second should be the smaller component")
}

func TestConnectedComponents_MembersAreSorted(t *testing.T) {
	refs := map[string][]string{
		"t1": {"zebra", "apple", "mango"},
	}

	components := ConnectedComponents(refs)
	require.Len(t, components, 1)
	assert.Equal(t, []string{"apple", "mango", "zebra"}, components[0])
}

// ---------------------------------------------------------------------------
// buildProjection tests
// ---------------------------------------------------------------------------

func TestBuildProjection_Basic(t *testing.T) {
	refs := map[string][]string{
		"t1": {"a", "b"},
		"t2": {"b", "c"},
	}

	adj, nodeIndex, indexToNode := buildProjection(refs)

	assert.Len(t, nodeIndex, 3, "should have 3 nodes")
	assert.Len(t, indexToNode, 3)

	// a-b connected (via t1), b-c connected (via t2), a-c not directly connected
	aIdx := nodeIndex["a"]
	bIdx := nodeIndex["b"]
	cIdx := nodeIndex["c"]

	assert.Equal(t, 1.0, adj[aIdx][bIdx], "a-b should be connected with weight 1")
	assert.Equal(t, 1.0, adj[bIdx][aIdx], "b-a should be connected with weight 1")
	assert.Equal(t, 1.0, adj[bIdx][cIdx], "b-c should be connected with weight 1")
	assert.Equal(t, 0.0, adj[aIdx][cIdx], "a-c should not be directly connected")
}

func TestBuildProjection_SharedMultipleTokens(t *testing.T) {
	// a and b share two tokens → weight should be 2
	refs := map[string][]string{
		"t1": {"a", "b"},
		"t2": {"a", "b"},
	}

	adj, nodeIndex, _ := buildProjection(refs)
	aIdx := nodeIndex["a"]
	bIdx := nodeIndex["b"]

	assert.Equal(t, 2.0, adj[aIdx][bIdx], "shared 2 tokens → weight 2")
}

func TestBuildProjection_NoEdgesForSingletons(t *testing.T) {
	refs := map[string][]string{
		"t1": {"a"},
	}

	adj, _, _ := buildProjection(refs)
	assert.Len(t, adj, 1)
	assert.Empty(t, adj[0], "singleton should have no edges")
}

// ---------------------------------------------------------------------------
// Modularity computation test
// ---------------------------------------------------------------------------

func TestModularity_PerfectPartition(t *testing.T) {
	// Two completely disconnected cliques
	refs := map[string][]string{
		"t1": {"a", "b"},
		"t2": {"c", "d"},
	}

	adj, nodeIndex, _ := buildProjection(refs)
	n := len(nodeIndex)

	// Assign perfect partition: {a,b} = community 0, {c,d} = community 1
	community := make([]int, n)
	community[nodeIndex["a"]] = 0
	community[nodeIndex["b"]] = 0
	community[nodeIndex["c"]] = 1
	community[nodeIndex["d"]] = 1

	degree := make([]float64, n)
	totalWeight := 0.0
	for i, neighbors := range adj {
		for _, w := range neighbors {
			degree[i] += w
		}
		totalWeight += degree[i]
	}

	mod := computeModularity(adj, community, degree, totalWeight, n)
	assert.Greater(t, mod, 0.4, "perfect partition of disjoint cliques should have high modularity")
}

func TestModularity_AllSameCommunity(t *testing.T) {
	refs := map[string][]string{
		"t1": {"a", "b"},
		"t2": {"c", "d"},
	}

	adj, nodeIndex, _ := buildProjection(refs)
	n := len(nodeIndex)

	// All in same community
	community := make([]int, n) // all zeros

	degree := make([]float64, n)
	totalWeight := 0.0
	for i, neighbors := range adj {
		for _, w := range neighbors {
			degree[i] += w
		}
		totalWeight += degree[i]
	}

	mod := computeModularity(adj, community, degree, totalWeight, n)
	assert.Equal(t, 0.0, mod, "all nodes in same community should have modularity 0")
}
