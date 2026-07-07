package graph

import (
	"sort"
)

// Community represents a cluster of densely-connected nodes detected by Louvain.
type Community struct {
	ID      int      // Community identifier
	Members []string // Node IDs belonging to this community
}

// CommunityResult holds the output of community detection.
type CommunityResult struct {
	Communities []Community    // Detected communities, sorted by size descending
	Membership  map[string]int // Node ID → community ID
	Modularity  float64        // Final modularity score (0 to 1, higher = better partition)
	NumNodes    int            // Total nodes in the graph
	NumEdges    int            // Total edges (undirected)
}

// DetectCommunities runs Louvain community detection on a bipartite refs graph.
// Input: refs maps token → []nodeID (which nodes reference that token).
// The algorithm projects this into a unipartite graph where two nodes are connected
// if they share at least one token, with edge weight = number of shared tokens.
// Returns communities of nodes that are densely co-referencing.
//
// minCommunitySize filters out communities smaller than this (default 2 if 0).
func DetectCommunities(refs map[string][]string, minCommunitySize int) *CommunityResult {
	if minCommunitySize <= 0 {
		minCommunitySize = 2
	}

	// Step 1: Build unipartite projection from bipartite refs graph.
	// Two nodes are connected if they share a token. Weight = # shared tokens.
	adj, nodeIndex, indexToNode := buildProjection(refs)
	n := len(nodeIndex)
	if n == 0 {
		return &CommunityResult{}
	}

	// Step 2: Pre-compute node degrees (static — adjacency never changes)
	// and total edge weight (2*m in modularity formula).
	degree := make([]float64, n)
	totalWeight := 0.0
	for i, neighbors := range adj {
		for _, w := range neighbors {
			degree[i] += w
		}
		totalWeight += degree[i]
	}
	if totalWeight == 0 {
		return &CommunityResult{}
	}

	// Step 3: Initialize — each node in its own community.
	// Also build commDegree: sum of degrees for each community,
	// maintained incrementally to avoid O(n) scans per candidate.
	community := make([]int, n)
	commDegree := make(map[int]float64, n)
	for i := range community {
		community[i] = i
		commDegree[i] = degree[i]
	}

	// Step 4: Louvain phase 1 — local modularity optimization
	improved := true
	for improved {
		improved = false
		for node := range n {
			bestComm := community[node]
			bestDelta := 0.0
			ki := degree[node]

			// Remove node from its community temporarily
			oldComm := community[node]
			community[node] = -1
			commDegree[oldComm] -= ki

			// Try each neighboring community
			neighborComms := make(map[int]float64) // community → sum of edge weights to that community
			for neighbor, w := range adj[node] {
				c := community[neighbor]
				if c >= 0 {
					neighborComms[c] += w
				}
			}

			// Also consider staying in original community
			if _, ok := neighborComms[oldComm]; !ok {
				neighborComms[oldComm] = 0
			}

			// Iterate candidate communities in sorted order with a strict `>`
			// so ΔQ ties break deterministically (lowest community index wins),
			// not by Go map iteration order. (mache-ff7e31)
			candidateComms := make([]int, 0, len(neighborComms))
			for c := range neighborComms {
				candidateComms = append(candidateComms, c)
			}
			sort.Ints(candidateComms)
			for _, c := range candidateComms {
				sigmaTotal := commDegree[c]
				delta := deltaModularity(neighborComms[c], sigmaTotal, ki, totalWeight)
				if delta > bestDelta {
					bestDelta = delta
					bestComm = c
				}
			}

			community[node] = bestComm
			commDegree[bestComm] += ki
			if bestComm != oldComm {
				improved = true
			}
		}
	}

	// Step 4.5: Member-gated connected-components post-split.
	// Louvain phase-1 local moving can leave a community internally
	// disconnected — its members split into pieces that only connect through
	// nodes OUTSIDE the community (Traag et al. 2019, "From Louvain to
	// Leiden"). Because a community is the sheaf cache-invalidation unit, a
	// disconnected one is a correctness hazard. Split any such community into
	// its connected components: monotonically finer, deterministic, split-only
	// (never merges), and NOT full Leiden (no coarsening/aggregation, no
	// randomness). Runs BEFORE minCommunitySize filtering and BEFORE the final
	// modularity computation so both reflect the post-split partition.
	community = splitDisconnectedCommunities(adj, community, n)

	// Step 5: Collect results
	commMap := map[int][]string{}
	for idx, c := range community {
		commMap[c] = append(commMap[c], indexToNode[idx])
	}

	// Renumber communities and filter by size
	var communities []Community
	id := 0
	membership := make(map[string]int, n)
	for _, members := range commMap {
		if len(members) < minCommunitySize {
			continue
		}
		sort.Strings(members)
		comm := Community{ID: id, Members: members}
		for _, m := range members {
			membership[m] = id
		}
		communities = append(communities, comm)
		id++
	}

	// Sort by size descending, breaking ties on the (already-sorted) first
	// member — a TOTAL order, so equal-sized communities get stable, input-
	// order-independent IDs. `sort.Slice` alone would preserve the random
	// commMap iteration order on ties. Members is non-empty (len >=
	// minCommunitySize >= 1). (mache-ff7e31)
	sort.Slice(communities, func(i, j int) bool {
		if len(communities[i].Members) != len(communities[j].Members) {
			return len(communities[i].Members) > len(communities[j].Members)
		}
		return communities[i].Members[0] < communities[j].Members[0]
	})
	// Re-assign IDs after sort
	for i := range communities {
		communities[i].ID = i
		for _, m := range communities[i].Members {
			membership[m] = i
		}
	}

	// Compute final modularity
	mod := computeModularity(adj, community, degree, totalWeight, n)

	numEdges := 0
	for _, neighbors := range adj {
		numEdges += len(neighbors)
	}
	numEdges /= 2 // undirected

	return &CommunityResult{
		Communities: communities,
		Membership:  membership,
		Modularity:  mod,
		NumNodes:    n,
		NumEdges:    numEdges,
	}
}

// buildProjection converts bipartite refs (token→[]nodeID) into an undirected
// weighted adjacency list (nodeIndex→{neighborIndex: weight}).
func buildProjection(refs map[string][]string) ([]map[int]float64, map[string]int, []string) {
	// Assign integer indices to nodes. Iterate tokens in SORTED order so the
	// node→index mapping is a pure function of `refs`, not of Go map iteration
	// order — otherwise the whole partition (which is keyed on these indices)
	// varies run-to-run on identical input. (mache-ff7e31)
	tokens := make([]string, 0, len(refs))
	for tok := range refs {
		tokens = append(tokens, tok)
	}
	sort.Strings(tokens)

	nodeIndex := make(map[string]int)
	var indexToNode []string
	for _, tok := range tokens {
		for _, n := range refs[tok] {
			if _, ok := nodeIndex[n]; !ok {
				nodeIndex[n] = len(indexToNode)
				indexToNode = append(indexToNode, n)
			}
		}
	}

	numNodes := len(nodeIndex)
	adj := make([]map[int]float64, numNodes)
	for i := range adj {
		adj[i] = make(map[int]float64)
	}

	// For each token, all pairs of nodes that share it get an edge
	for _, nodes := range refs {
		if len(nodes) < 2 {
			continue
		}
		indices := make([]int, len(nodes))
		for i, n := range nodes {
			indices[i] = nodeIndex[n]
		}
		for i := range indices {
			for j := i + 1; j < len(indices); j++ {
				a, b := indices[i], indices[j]
				if a != b {
					adj[a][b]++
					adj[b][a]++
				}
			}
		}
	}

	return adj, nodeIndex, indexToNode
}

// deltaModularity computes the change in modularity from moving a node into a community.
// kiIn: sum of weights from node to nodes in target community
// sigmaTotal: sum of degrees of nodes in target community
// ki: degree of the node being moved
// m2: total weight of all edges (sum of adjacency matrix)
func deltaModularity(kiIn, sigmaTotal, ki, m2 float64) float64 {
	return kiIn/m2 - (sigmaTotal*ki)/(m2*m2)
}

// computeModularity calculates the modularity of a partition in O(E).
//
// Algebraically identical to the textbook O(N^2) double sum
//
//	Q = (1/2m) * sum_ij [ A_ij - (ki*kj)/(2m) ] * delta(ci, cj)
//
// but grouped per community so it needs a single pass over the edges plus one
// over the nodes:
//
//	Q = sum_c [ inW_c/m2 - (commDegree_c/m2)^2 ]
//
// where inW_c is the total weight of edges internal to community c (both
// directions, matching the ordered-pair double sum) and commDegree_c is the
// sum of node degrees in c. m2 is the total edge weight (2m).
func computeModularity(adj []map[int]float64, community []int, degree []float64, m2 float64, n int) float64 {
	if m2 == 0 {
		return 0
	}
	inW := make(map[int]float64, n)     // internal edge weight per community
	commDeg := make(map[int]float64, n) // summed degree per community
	for i := range n {
		c := community[i]
		commDeg[c] += degree[i]
		for j, w := range adj[i] {
			if community[j] == c {
				inW[c] += w
			}
		}
	}
	q := 0.0
	for c, cd := range commDeg {
		frac := cd / m2
		q += inW[c]/m2 - frac*frac // inW[c] is 0 for edgeless communities
	}
	return q
}

// splitDisconnectedCommunities enforces the invariant that every returned
// community is internally connected. For each community it induces the subgraph
// over ONLY that community's members (edges to out-of-community nodes are
// ignored) and runs BFS to find connected components; a community with more
// than one component is relabeled into one community per component.
//
// The pass is split-only (it never merges two communities), deterministic
// (communities and BFS seeds are visited in sorted order, so component labels
// are stable), and monotonically finer than the input partition. It reuses the
// same BFS connectivity oracle as ConnectedComponents, gated to the member set.
// Returns a fresh label slice; the input community slice is not mutated.
func splitDisconnectedCommunities(adj []map[int]float64, community []int, n int) []int {
	// Group node indices by their current community label.
	members := make(map[int][]int)
	for i := range n {
		members[community[i]] = append(members[community[i]], i)
	}

	labels := make([]int, 0, len(members))
	for c := range members {
		labels = append(labels, c)
	}
	sort.Ints(labels)

	result := make([]int, n)
	visited := make([]bool, n)
	nextLabel := 0
	for _, c := range labels {
		memberSet := make(map[int]bool, len(members[c]))
		for _, idx := range members[c] {
			memberSet[idx] = true
		}
		seeds := members[c]
		sort.Ints(seeds) // deterministic component labeling
		for _, start := range seeds {
			if visited[start] {
				continue
			}
			label := nextLabel
			nextLabel++
			queue := []int{start}
			visited[start] = true
			for len(queue) > 0 {
				node := queue[0]
				queue = queue[1:]
				result[node] = label
				for neighbor := range adj[node] {
					if memberSet[neighbor] && !visited[neighbor] {
						visited[neighbor] = true
						queue = append(queue, neighbor)
					}
				}
			}
		}
	}
	return result
}

// ConnectedComponents finds connected components in the refs graph projection.
// Simpler than Louvain — useful as a baseline or when modularity optimization
// is overkill (e.g., disconnected subgraphs).
func ConnectedComponents(refs map[string][]string) [][]string {
	adj, _, indexToNode := buildProjection(refs)
	n := len(indexToNode)
	if n == 0 {
		return nil
	}

	visited := make([]bool, n)
	var components [][]string

	for i := range n {
		if visited[i] {
			continue
		}
		// BFS from node i
		var component []string
		queue := []int{i}
		visited[i] = true
		for len(queue) > 0 {
			node := queue[0]
			queue = queue[1:]
			component = append(component, indexToNode[node])
			for neighbor := range adj[node] {
				if !visited[neighbor] {
					visited[neighbor] = true
					queue = append(queue, neighbor)
				}
			}
		}
		sort.Strings(component)
		components = append(components, component)
	}

	// Sort by size descending
	sort.Slice(components, func(i, j int) bool {
		return len(components[i]) > len(components[j])
	})

	return components
}
