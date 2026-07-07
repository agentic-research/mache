package graph

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// countComponentsInSubset does a BFS over adj (the EXACT projection
// DetectCommunities used) restricted to `members`. Returns the number of
// connected components among the member nodes — the Leiden "internally
// disconnected community" oracle applied to one community. members are node
// indices from buildProjection's nodeIndex.
//
// Ported from the preserved community-disconnected experiment harness.
func countComponentsInSubset(adj []map[int]float64, members map[int]bool) int {
	visited := make(map[int]bool, len(members))
	components := 0
	for start := range members {
		if visited[start] {
			continue
		}
		components++
		queue := []int{start}
		visited[start] = true
		for len(queue) > 0 {
			node := queue[0]
			queue = queue[1:]
			for neighbor := range adj[node] {
				if members[neighbor] && !visited[neighbor] {
					visited[neighbor] = true
					queue = append(queue, neighbor)
				}
			}
		}
	}
	return components
}

// computeModularityQuadratic is the original O(N^2) reference implementation,
// kept verbatim so the O(E) computeModularity can be parity-tested against it.
//
//	Q = (1/2m) * sum_ij [ A_ij - (ki*kj)/(2m) ] * delta(ci, cj)
func computeModularityQuadratic(adj []map[int]float64, community []int, degree []float64, m2 float64, n int) float64 {
	if m2 == 0 {
		return 0
	}
	q := 0.0
	for i := range n {
		ki := degree[i]
		for j := range n {
			if community[i] != community[j] {
				continue
			}
			kj := degree[j]
			aij := adj[i][j] // 0 if no edge
			q += aij - (ki*kj)/m2
		}
	}
	return q / m2
}

// degreesAndTotal computes the weighted degree slice and total edge weight (2m)
// for an adjacency list.
func degreesAndTotal(adj []map[int]float64, n int) ([]float64, float64) {
	degree := make([]float64, n)
	total := 0.0
	for i, neighbors := range adj {
		for _, w := range neighbors {
			degree[i] += w
		}
		total += degree[i]
	}
	return degree, total
}

// ---------------------------------------------------------------------------
// Invariant / regression gate: no returned community spans >1 connected
// component. Passes trivially on today's dense projections; becomes the guard
// the moment the input graph sparsifies enough for Louvain phase-1 to emit an
// internally disconnected community.
// ---------------------------------------------------------------------------

// buildClusteredRefs synthesizes a "real-ish" refs graph: `numClusters`
// clusters, each of `clusterSize` nodes densely co-referencing two private
// tokens, plus a single weak bridge token linking one node of each adjacent
// cluster pair. This is exactly the topology Louvain must resolve — dense
// cores with weak inter-cluster ties — without hand-tuning it to any particular
// partition.
func buildClusteredRefs(numClusters, clusterSize int) map[string][]string {
	refs := map[string][]string{}
	node := func(c, i int) string { return fmt.Sprintf("c%d_n%d", c, i) }
	for c := range numClusters {
		var members []string
		for i := range clusterSize {
			members = append(members, node(c, i))
		}
		refs[fmt.Sprintf("tok%d_a", c)] = members
		refs[fmt.Sprintf("tok%d_b", c)] = members
		// weak bridge to the next cluster (wraps around)
		next := (c + 1) % numClusters
		refs[fmt.Sprintf("bridge_%d_%d", c, next)] = []string{node(c, 0), node(next, 0)}
	}
	return refs
}

func TestCommunities_NoDisconnectedCommunity_Invariant(t *testing.T) {
	refs := buildClusteredRefs(6, 7)

	result := DetectCommunities(refs, 2)
	require.NotNil(t, result)
	require.NotEmpty(t, result.Communities)

	// Rebuild the SAME projection the detector used so the induced-subgraph
	// oracle sees identical adjacency (weights included).
	adj, nodeIndex, _ := buildProjection(refs)

	for _, c := range result.Communities {
		members := make(map[int]bool, len(c.Members))
		for _, m := range c.Members {
			idx, ok := nodeIndex[m]
			require.True(t, ok, "member %s missing from projection", m)
			members[idx] = true
		}
		comps := countComponentsInSubset(adj, members)
		assert.Equalf(t, 1, comps,
			"community #%d (%d members) spans %d connected components — internally disconnected",
			c.ID, len(c.Members), comps)
	}
}

// ---------------------------------------------------------------------------
// Planted disconnection: a barbell where the bridge node is pulled into a third
// community, leaving the two end-clusters in one community with no internal
// edge between them. The post-split pass must separate them into the two real
// pieces.
// ---------------------------------------------------------------------------

func barbellRefs() map[string][]string {
	// Two dense cliques L={l1,l2,l3} and R={r1,r2,r3} with no shared token.
	// Bridge node x links to l1 and r1 through private tokens.
	return map[string][]string{
		"lt1":     {"l1", "l2", "l3"},
		"lt2":     {"l1", "l2", "l3"},
		"rt1":     {"r1", "r2", "r3"},
		"rt2":     {"r1", "r2", "r3"},
		"bridgeL": {"x", "l1"},
		"bridgeR": {"x", "r1"},
	}
}

func TestSplitDisconnectedCommunities_Barbell(t *testing.T) {
	refs := barbellRefs()
	adj, nodeIndex, _ := buildProjection(refs)
	n := len(nodeIndex)

	// Plant the pathological partition Louvain phase-1 can produce: both end
	// clusters share community 0 while the bridge node x sits in community 1.
	// Community 0 is then internally disconnected (L and R share no edge).
	community := make([]int, n)
	lCluster := []string{"l1", "l2", "l3"}
	rCluster := []string{"r1", "r2", "r3"}
	for _, node := range append(append([]string{}, lCluster...), rCluster...) {
		community[nodeIndex[node]] = 0
	}
	community[nodeIndex["x"]] = 1

	// Sanity: the planted community 0 really is disconnected (2 components).
	planted := map[int]bool{}
	for _, node := range append(append([]string{}, lCluster...), rCluster...) {
		planted[nodeIndex[node]] = true
	}
	require.Equal(t, 2, countComponentsInSubset(adj, planted),
		"planted community 0 must be internally disconnected for this test to mean anything")

	split := splitDisconnectedCommunities(adj, community, n)

	// L-cluster stays together, R-cluster stays together, and they are now in
	// DIFFERENT communities — split-only, never merge.
	lLabel := split[nodeIndex["l1"]]
	for _, node := range lCluster {
		assert.Equal(t, lLabel, split[nodeIndex[node]], "L-cluster must stay intact")
	}
	rLabel := split[nodeIndex["r1"]]
	for _, node := range rCluster {
		assert.Equal(t, rLabel, split[nodeIndex[node]], "R-cluster must stay intact")
	}
	assert.NotEqual(t, lLabel, rLabel, "split must separate the two disconnected pieces")

	// The bridge node was already its own (connected) community — untouched.
	assert.NotEqual(t, lLabel, split[nodeIndex["x"]])
	assert.NotEqual(t, rLabel, split[nodeIndex["x"]])
}

func TestSplitDisconnectedCommunities_ConnectedIsUntouched(t *testing.T) {
	// A single connected community must remain a single community after split.
	refs := map[string][]string{
		"t1": {"a", "b"},
		"t2": {"b", "c"},
		"t3": {"c", "d"},
	}
	adj, nodeIndex, _ := buildProjection(refs)
	n := len(nodeIndex)
	community := make([]int, n) // all in community 0, and they ARE connected

	split := splitDisconnectedCommunities(adj, community, n)
	label := split[0]
	for i := range n {
		assert.Equal(t, label, split[i], "connected community must not be split")
	}
}

func TestDetectCommunities_BarbellEndToEnd_AllConnected(t *testing.T) {
	refs := barbellRefs()
	result := DetectCommunities(refs, 2)
	require.NotNil(t, result)

	adj, nodeIndex, _ := buildProjection(refs)
	for _, c := range result.Communities {
		members := make(map[int]bool, len(c.Members))
		for _, m := range c.Members {
			members[nodeIndex[m]] = true
		}
		assert.Equalf(t, 1, countComponentsInSubset(adj, members),
			"community #%d must be internally connected end-to-end", c.ID)
	}
}

// ---------------------------------------------------------------------------
// Modularity parity: the O(E) computeModularity must equal the O(N^2)
// reference within float epsilon across several small graphs and partitions.
// ---------------------------------------------------------------------------

func TestModularityParity_ONvsONE(t *testing.T) {
	fixtures := map[string]map[string][]string{
		"two-cliques": {
			"t1": {"a", "b"},
			"t2": {"c", "d"},
		},
		"chain": {
			"t1": {"a", "b"},
			"t2": {"b", "c"},
			"t3": {"c", "d"},
		},
		"multi-package": {
			"ValidateToken": {"auth/handler", "auth/middleware", "auth/service"},
			"ParseJWT":      {"auth/handler", "auth/service"},
			"HandleRequest": {"api/router", "api/handler", "api/middleware"},
			"OpenConn":      {"db/pool", "db/migrate", "db/repository"},
			"Logger":        {"auth/service", "api/middleware", "db/pool"},
		},
		"clustered": buildClusteredRefs(4, 5),
	}

	for name, refs := range fixtures {
		t.Run(name, func(t *testing.T) {
			adj, _, _ := buildProjection(refs)
			n := len(adj)
			degree, m2 := degreesAndTotal(adj, n)
			require.Positive(t, n)

			// Several partitions to exercise the formula:
			partitions := map[string][]int{}
			// (1) every node in its own community
			own := make([]int, n)
			for i := range n {
				own[i] = i
			}
			partitions["singletons"] = own
			// (2) all nodes in one community
			partitions["all-one"] = make([]int, n)
			// (3) alternating 2-coloring
			alt := make([]int, n)
			for i := range n {
				alt[i] = i % 2
			}
			partitions["alternating"] = alt
			// (4) blocks of 3
			blocks := make([]int, n)
			for i := range n {
				blocks[i] = i / 3
			}
			partitions["blocks-of-3"] = blocks
			// (5) the actual detector partition
			result := DetectCommunities(refs, 2)
			detected := make([]int, n)
			// map via a fresh projection index — rebuild to align indices
			_, nodeIndex, _ := buildProjection(refs)
			for _, c := range result.Communities {
				for _, m := range c.Members {
					detected[nodeIndex[m]] = c.ID + 1 // +1 keeps filtered singletons in community 0
				}
			}
			partitions["detected"] = detected

			for pname, community := range partitions {
				want := computeModularityQuadratic(adj, community, degree, m2, n)
				got := computeModularity(adj, community, degree, m2, n)
				assert.InDeltaf(t, want, got, 1e-9,
					"partition %q: O(E) modularity %.12f != O(N^2) %.12f", pname, got, want)
			}
		})
	}
}
