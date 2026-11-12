package graph

import (
	"maps"
	"slices"
	"sort"
)

// Community represents a cluster of densely-connected nodes detected by Louvain.
type Community struct {
	ID      int      // Community identifier
	Members []string // Node IDs belonging to this community
}

// CommunityResult holds the output of community detection.
type CommunityResult struct {
	Communities []Community    // Detected communities, in lessBySizeThenFirst order
	Membership  map[string]int // Node ID → community ID
	// Modularity is the final modularity score. It lies in [-1/2, 1), not
	// [0, 1]: negative when the communities hold less internal weight than
	// a random graph with the same degrees would, 0 when no better than
	// chance, and 1 approached but never reached. Higher is a better
	// partition.
	Modularity float64
	NumNodes   int // Total nodes in the graph
	NumEdges   int // Total edges (undirected)
	// PrunedTokens are hub tokens excluded from the projection, most-referenced
	// first. Reported rather than dropped silently: a caller reading these
	// communities is entitled to know which references did not shape them.
	PrunedTokens []PrunedToken
	// EffectiveMaxFanIn is the cap actually applied. It differs from the
	// requested one when the pair budget forced a tighter cap — reported so a
	// caller can tell "these are your communities" from "these are your
	// communities under a budget I had to impose".
	EffectiveMaxFanIn int
	// ProjectedPairs is the edge-insertion count the projection performed.
	ProjectedPairs int
}

// PrunedToken is a token excluded for exceeding the fan-in cap.
type PrunedToken struct {
	Token string `json:"token"`
	FanIn int    `json:"fan_in"`
}

// DefaultMaxFanIn is the per-token fan-in above which a token is treated as a
// hub and excluded from the co-reference projection.
//
// The projection connects every PAIR of nodes sharing a token, so a token
// referenced by K nodes costs K(K-1)/2 edges. Measured on mache itself: 12,275
// tokens produce 84,813,280 edge insertions, and the single token `t` (the
// ubiquitous *testing.T receiver, K=9,903) accounts for 49M of them — 58% of
// the total. The top ten tokens — t, err, NoError, require.NoError, Equal,
// len — are 86%.
//
// Cost is only half the reason. Those hubs also DESTROY the answer: `t` joins
// 9,903 nodes into a near-complete subgraph, so Louvain collapses everything
// that touches *testing.T into one blob. A token that nearly everything
// references is not evidence of shared purpose; it is the co-occurrence
// equivalent of a stopword.
//
// 100 keeps 12,091 of 12,275 tokens (98.5%) and removes 99.2% of the edge
// work. The threshold is a heuristic, exposed as a parameter — it is not a
// constant anyone should have to edit source to change, which is exactly the
// mistake the old refs-count cap made.
const DefaultMaxFanIn = 100

// DefaultMaxPairs bounds the projection's total edge insertions — Σ K(K-1)/2
// over surviving tokens — as a backstop behind the fan-in cap.
//
// Fan-in pruning removes the DOMINANT term, not the whole cost. With a cap of
// 100, a pathological repo where every one of 12,275 tokens sits exactly at the
// cap would still project 60.8M pairs; mache measures 681K only because real
// fan-in is heavily skewed. A budget keyed on the quantity that actually costs
// is the guard the old refs-count cap was reaching for and missed by ~7000×.
//
// 5,000,000 is ~7x mache's own measured cost, so a repo an order of magnitude
// larger still runs unclamped.
const DefaultMaxPairs = 5_000_000

// DetectCommunities runs Louvain community detection on a bipartite refs graph.
// Input: refs maps token → []nodeID (which nodes reference that token).
// The algorithm projects this into a unipartite graph where two nodes are connected
// if they share at least one token, with edge weight = number of shared tokens.
// Returns communities of nodes that are densely co-referencing.
//
// minCommunitySize filters out communities smaller than this (default 2 if 0).
func DetectCommunities(refs map[string][]string, minCommunitySize int) *CommunityResult {
	return DetectCommunitiesWithFanIn(refs, minCommunitySize, DefaultMaxFanIn)
}

// DetectCommunitiesWithFanIn is DetectCommunities with an explicit hub cap.
// maxFanIn <= 0 disables pruning and restores the pre-pruning behaviour, which
// is quadratic in the hottest token's fan-in — see DefaultMaxFanIn.
func DetectCommunitiesWithFanIn(refs map[string][]string, minCommunitySize, maxFanIn int) *CommunityResult {
	if minCommunitySize <= 0 {
		minCommunitySize = 2
	}
	maxFanIn = fitFanInToBudget(refs, maxFanIn, DefaultMaxPairs)
	refs, pruned := pruneHubTokens(refs, maxFanIn)
	projected := projectedPairs(refs)

	// Step 1: Build unipartite projection from bipartite refs graph.
	// Two nodes are connected if they share a token. Weight = # shared tokens.
	adj, nodeIndex, indexToNode := buildProjection(refs)
	n := len(nodeIndex)
	if n == 0 {
		return &CommunityResult{PrunedTokens: pruned, EffectiveMaxFanIn: maxFanIn, ProjectedPairs: projected}
	}

	// Step 2: Pre-compute node degrees (static — adjacency never changes)
	// and total edge weight (2*m in modularity formula).
	degree, totalWeight := nodeDegrees(adj)
	if totalWeight == 0 {
		return &CommunityResult{PrunedTokens: pruned, EffectiveMaxFanIn: maxFanIn, ProjectedPairs: projected}
	}

	// Steps 3 and 4: Louvain phase 1 from the singleton partition.
	community := louvainPhase1(adj, degree, totalWeight)

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
	communities, membership := collectCommunities(community, indexToNode, minCommunitySize)

	// Compute final modularity
	mod := computeModularity(adj, community, degree, totalWeight, n)

	numEdges := 0
	for _, neighbors := range adj {
		numEdges += len(neighbors)
	}
	numEdges /= 2 // undirected

	return &CommunityResult{
		Communities:  communities,
		Membership:   membership,
		Modularity:   mod,
		NumNodes:     n,
		NumEdges:     numEdges,
		PrunedTokens: pruned,

		EffectiveMaxFanIn: maxFanIn,
		ProjectedPairs:    projected,
	}
}

// lessBySizeThenFirst is the order every member list this package returns is
// sorted in: larger first, ties broken on the smallest member. Both operands
// must be sorted and non-empty, which holds for every caller (members are
// sorted on collection, and a community or component has at least one node).
//
// It is a TOTAL order because two distinct communities or components are
// disjoint, so their smallest members differ. Size alone is not: `sort.Slice`
// is unstable and the input order on ties is a map walk, so two equal-sized
// groups could swap between runs on identical input, and with them the IDs a
// caller keys on (mache-82ded3).
func lessBySizeThenFirst(a, b []string) bool {
	if len(a) != len(b) {
		return len(a) > len(b)
	}
	return a[0] < b[0]
}

// nodeDegrees is each node's weighted degree and their total (2m).
//
// This sum, and the neighbour-community sum in louvainPhase1, run in Go's
// randomized map order. That is safe ONLY because every weight is a count of
// shared tokens: an exact small integer in a float64, so the sum is exact in
// any order. A fractional weight would make these sums order-dependent and
// the partition non-deterministic — sort the keys (as computeModularity does)
// before introducing one (mache-82ded3).
func nodeDegrees(adj []map[int]float64) ([]float64, float64) {
	degree := make([]float64, len(adj))
	totalWeight := 0.0
	for i, neighbors := range adj {
		for _, w := range neighbors {
			degree[i] += w
		}
		totalWeight += degree[i]
	}
	return degree, totalWeight
}

// louvainPhase1 is Louvain's local-moving phase: starting from every node in
// its own community, each node moves to the neighbouring community with the
// best modularity gain until no move improves. Returns the community label of
// each node index; labels are node indices, not yet renumbered.
func louvainPhase1(adj []map[int]float64, degree []float64, totalWeight float64) []int {
	n := len(adj)
	// Each node in its own community. commDegree is the summed degree per
	// community, maintained incrementally to avoid O(n) scans per candidate.
	community := make([]int, n)
	commDegree := make(map[int]float64, n)
	for i := range community {
		community[i] = i
		commDegree[i] = degree[i]
	}

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

			// Try each neighboring community. Exact-integer weights make
			// this map-order sum order-independent (see nodeDegrees).
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
	return community
}

// collectCommunities turns per-node labels into the returned communities:
// members named and sorted, communities under minCommunitySize dropped, the
// rest in lessBySizeThenFirst order with IDs assigned in that order, and the
// node → ID membership map to match.
//
// The order is TOTAL, so equal-sized communities get stable, input-order-
// independent IDs; sorting by size alone would preserve the random map
// iteration order on ties. (mache-ff7e31)
func collectCommunities(community []int, indexToNode []string, minCommunitySize int) ([]Community, map[string]int) {
	commMap := map[int][]string{}
	for idx, c := range community {
		commMap[c] = append(commMap[c], indexToNode[idx])
	}

	var communities []Community
	for _, members := range commMap {
		if len(members) < minCommunitySize {
			continue
		}
		sort.Strings(members)
		communities = append(communities, Community{Members: members})
	}
	sort.Slice(communities, func(i, j int) bool {
		return lessBySizeThenFirst(communities[i].Members, communities[j].Members)
	})

	membership := make(map[string]int, len(community))
	for i := range communities {
		communities[i].ID = i
		for _, m := range communities[i].Members {
			membership[m] = i
		}
	}
	return communities, membership
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

	// For each token, all pairs of nodes that share it get an edge. The
	// token order is the sorted one from above, so the whole projection is a
	// pure function of `refs` by construction, not only because the `++`
	// increments happen to be exact in any order (mache-82ded3).
	for _, tok := range tokens {
		nodes := refs[tok]
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
	// Every sum runs in sorted key order. Float addition is not associative,
	// so summing in Go's randomized map order gave the same partition a
	// different modularity in the last digits run to run (mache-a76e43).
	inW := make(map[int]float64, n)     // internal edge weight per community
	commDeg := make(map[int]float64, n) // summed degree per community
	for i := range n {
		c := community[i]
		commDeg[c] += degree[i]
		for _, j := range slices.Sorted(maps.Keys(adj[i])) {
			if community[j] == c {
				inW[c] += adj[i][j]
			}
		}
	}
	q := 0.0
	for _, c := range slices.Sorted(maps.Keys(commDeg)) {
		frac := commDeg[c] / m2
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
// is overkill (e.g., disconnected subgraphs). Each component's members are
// sorted and the components come back in lessBySizeThenFirst order, so the
// result is a pure function of refs.
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

	sort.Slice(components, func(i, j int) bool {
		return lessBySizeThenFirst(components[i], components[j])
	})

	return components
}

// pruneHubTokens removes tokens referenced by more than maxFanIn nodes and
// returns the survivors plus what was dropped, most-referenced first.
//
// Returns refs unchanged when maxFanIn <= 0 or nothing exceeds it, so the
// common case allocates nothing.
func pruneHubTokens(refs map[string][]string, maxFanIn int) (map[string][]string, []PrunedToken) {
	if maxFanIn <= 0 {
		return refs, nil
	}
	var pruned []PrunedToken
	for tok, nodes := range refs {
		if len(nodes) > maxFanIn {
			pruned = append(pruned, PrunedToken{Token: tok, FanIn: len(nodes)})
		}
	}
	if len(pruned) == 0 {
		return refs, nil
	}
	sort.Slice(pruned, func(i, j int) bool {
		if pruned[i].FanIn != pruned[j].FanIn {
			return pruned[i].FanIn > pruned[j].FanIn
		}
		return pruned[i].Token < pruned[j].Token // deterministic ties (mache-ff7e31)
	})
	kept := make(map[string][]string, len(refs)-len(pruned))
	for tok, nodes := range refs {
		if len(nodes) <= maxFanIn {
			kept[tok] = nodes
		}
	}
	return kept, pruned
}

// projectedPairs is the edge-insertion count buildProjection will perform:
// Σ K(K-1)/2 over tokens. O(tokens), so it is cheap enough to compute BEFORE
// deciding whether the projection is affordable.
func projectedPairs(refs map[string][]string) int {
	total := 0
	for _, nodes := range refs {
		k := len(nodes)
		if k > 1 {
			total += k * (k - 1) / 2
		}
	}
	return total
}

// fitFanInToBudget lowers maxFanIn until the projection fits maxPairs, and
// returns the cap that does.
//
// Tightening beats refusing. The old behaviour returned a note saying the graph
// was too large and NO communities, which is the least useful of the available
// answers — a caller asking "what clusters together" would rather have the
// answer computed under a stated cap than nothing at all. The cap actually used
// rides back on the result, so a clamped answer is never mistaken for an
// unclamped one.
//
// Deterministic: a pure function of refs and the budget, halving from the
// requested cap, so identical input yields an identical cap and therefore an
// identical partition (mache-ff7e31).
func fitFanInToBudget(refs map[string][]string, maxFanIn, maxPairs int) int {
	if maxFanIn <= 0 || maxPairs <= 0 {
		return maxFanIn // pruning or budgeting disabled — caller's explicit choice
	}
	for cap := maxFanIn; cap > 1; cap /= 2 {
		kept, _ := pruneHubTokens(refs, cap)
		if projectedPairs(kept) <= maxPairs {
			return cap
		}
	}
	return 1 // every remaining token connects at most one node: no pairs at all
}
