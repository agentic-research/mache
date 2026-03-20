package graph

import (
	"log"
)

// SheafInvalidator wraps a Graph with sheaf-aware cascading invalidation.
// When a node is invalidated, it looks up the node's community (region),
// asks the ley-line daemon which regions are transitively affected, then
// invalidates all nodes in those affected regions.
//
// If the SheafClient is nil or the daemon is unavailable, it falls back
// to plain Graph.Invalidate on the single node.
type SheafInvalidator struct {
	graph  Graph
	sheaf  SheafBackend
	result *CommunityResult
}

// SheafBackend is the subset of SheafClient that SheafInvalidator needs.
// Defined as an interface to allow testing without a real daemon.
type SheafBackend interface {
	Invalidate(regionID int) ([]int, error)
}

// NewSheafInvalidator creates a SheafInvalidator. All parameters are optional:
//   - graph may be nil (all operations become no-ops)
//   - sheaf may be nil (falls back to single-node invalidation)
//   - result may be nil (falls back to single-node invalidation)
func NewSheafInvalidator(graph Graph, sheaf SheafBackend, result *CommunityResult) *SheafInvalidator {
	return &SheafInvalidator{
		graph:  graph,
		sheaf:  sheaf,
		result: result,
	}
}

// SetCommunityResult updates the community detection result used for lookups.
// Call this after re-running community detection.
func (si *SheafInvalidator) SetCommunityResult(cr *CommunityResult) {
	si.result = cr
}

// InvalidateWithCascade invalidates a node and, if sheaf is available,
// cascades the invalidation to all nodes in transitively affected regions.
//
// The membership map is used to look up which community the node belongs to.
// If membership is nil, si.result.Membership is used.
//
// Returns the number of nodes invalidated.
func (si *SheafInvalidator) InvalidateWithCascade(id string, membership map[string]int) int {
	if si == nil || si.graph == nil {
		return 0
	}

	// Use stored membership if caller didn't provide one.
	if membership == nil && si.result != nil {
		membership = si.result.Membership
	}

	// If no sheaf backend or no community info, fall back to single invalidation.
	if si.sheaf == nil || membership == nil {
		si.graph.Invalidate(id)
		return 1
	}

	regionID, ok := membership[id]
	if !ok {
		// Node not in any community — just invalidate it directly.
		si.graph.Invalidate(id)
		return 1
	}

	affected, err := si.sheaf.Invalidate(regionID)
	if err != nil {
		// Daemon error — log and fall back to single invalidation.
		log.Printf("sheaf invalidate region %d: %v (falling back to single node)", regionID, err)
		si.graph.Invalidate(id)
		return 1
	}

	if len(affected) == 0 {
		// Daemon returned no affected regions — invalidate just the original.
		si.graph.Invalidate(id)
		return 1
	}

	// Build set of affected region IDs for fast lookup.
	affectedSet := make(map[int]struct{}, len(affected))
	for _, rid := range affected {
		affectedSet[rid] = struct{}{}
	}

	// Invalidate all nodes in the affected regions.
	count := 0
	for nodeID, cid := range membership {
		if _, hit := affectedSet[cid]; hit {
			si.graph.Invalidate(nodeID)
			count++
		}
	}

	return count
}
