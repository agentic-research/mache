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

	// Step 2: Compute total edge weight (2*m in modularity formula)
	totalWeight := 0.0
	for _, neighbors := range adj {
		for _, w := range neighbors {
			totalWeight += w
		}
	}
	if totalWeight == 0 {
		return &CommunityResult{}
	}

	// Step 3: Initialize — each node in its own community
	community := make([]int, n)
	for i := range community {
		community[i] = i
	}

	// Step 4: Louvain phase 1 — local modularity optimization
	improved := true
	for improved {
		improved = false
		for node := 0; node < n; node++ {
			bestComm := community[node]
			bestDelta := 0.0

			// Degree of this node (sum of edge weights)
			ki := nodeDegree(adj, node)

			// Remove node from its community temporarily
			oldComm := community[node]
			community[node] = -1

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

			for c, kiIn := range neighborComms {
				// Sum of weights of edges to nodes in community c
				sigmaTotal := communityDegree(adj, community, c, n)
				delta := deltaModularity(kiIn, sigmaTotal, ki, totalWeight)
				if delta > bestDelta {
					bestDelta = delta
					bestComm = c
				}
			}

			community[node] = bestComm
			if bestComm != oldComm {
				improved = true
			}
		}
	}

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

	// Sort by size descending
	sort.Slice(communities, func(i, j int) bool {
		return len(communities[i].Members) > len(communities[j].Members)
	})
	// Re-assign IDs after sort
	for i := range communities {
		communities[i].ID = i
		for _, m := range communities[i].Members {
			membership[m] = i
		}
	}

	// Compute final modularity
	mod := computeModularity(adj, community, totalWeight, n)

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
	// Assign integer indices to nodes
	nodeIndex := make(map[string]int)
	var indexToNode []string
	for _, nodes := range refs {
		for _, n := range nodes {
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
		for i := 0; i < len(indices); i++ {
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

// nodeDegree returns the weighted degree of a node.
func nodeDegree(adj []map[int]float64, node int) float64 {
	sum := 0.0
	for _, w := range adj[node] {
		sum += w
	}
	return sum
}

// communityDegree returns the sum of weighted degrees of all nodes in a community.
func communityDegree(adj []map[int]float64, community []int, comm, n int) float64 {
	sum := 0.0
	for i := 0; i < n; i++ {
		if community[i] == comm {
			sum += nodeDegree(adj, i)
		}
	}
	return sum
}

// deltaModularity computes the change in modularity from moving a node into a community.
// kiIn: sum of weights from node to nodes in target community
// sigmaTotal: sum of degrees of nodes in target community
// ki: degree of the node being moved
// m2: total weight of all edges (sum of adjacency matrix)
func deltaModularity(kiIn, sigmaTotal, ki, m2 float64) float64 {
	return kiIn/m2 - (sigmaTotal*ki)/(m2*m2)
}

// computeModularity calculates the modularity of a partition.
// Q = (1/2m) * sum_ij [ A_ij - (ki*kj)/(2m) ] * delta(ci, cj)
func computeModularity(adj []map[int]float64, community []int, m2 float64, n int) float64 {
	if m2 == 0 {
		return 0
	}
	q := 0.0
	for i := 0; i < n; i++ {
		ki := nodeDegree(adj, i)
		for j := 0; j < n; j++ {
			if community[i] != community[j] {
				continue
			}
			kj := nodeDegree(adj, j)
			aij := adj[i][j] // 0 if no edge
			q += aij - (ki*kj)/m2
		}
	}
	return q / m2
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

	for i := 0; i < n; i++ {
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
