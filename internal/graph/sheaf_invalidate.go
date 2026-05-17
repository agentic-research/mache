package graph

import (
	"log"
	"sync"
)

// SheafInvalidator wraps a Graph with sheaf-aware cascading invalidation.
// When a node is invalidated, it looks up the node's community (region),
// asks the ley-line daemon which regions are transitively affected, then
// invalidates all nodes in those affected regions.
//
// If the SheafClient is nil or the daemon is unavailable, it falls back
// to plain Graph.Invalidate on the single node.
type SheafInvalidator struct {
	graph Graph
	// mu guards sheaf + result. The watcher goroutine calls
	// InvalidateWithCascade while the MCP get_communities handler may
	// concurrently call SetCommunityResult / SetSheaf. Both reads
	// (sheaf, result) are taken under RLock; writes upgrade to Lock.
	mu     sync.RWMutex
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
	si.mu.Lock()
	defer si.mu.Unlock()
	si.result = cr
}

// SetSheaf swaps the sheaf backend. Pass nil to disable cascades (the
// invalidator falls back to single-node Graph.Invalidate). Used by the
// serve startup path to attach a SheafClient lazily — the file watcher
// can install the invalidator before the ley-line daemon is reachable,
// then swap in a real backend once a connection is established.
func (si *SheafInvalidator) SetSheaf(b SheafBackend) {
	si.mu.Lock()
	defer si.mu.Unlock()
	si.sheaf = b
}

// HasResult reports whether the invalidator has a CommunityResult to
// look up regions in. When false, InvalidateWithCascade degrades to
// single-node invalidate (still correct, just no cascade). Callers
// (e.g. the watcher) use this to decide whether to skip the lookup
// entirely vs. fall through.
func (si *SheafInvalidator) HasResult() bool {
	if si == nil {
		return false
	}
	si.mu.RLock()
	defer si.mu.RUnlock()
	return si.result != nil
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

	// Take a consistent snapshot of sheaf + result under the read lock,
	// then release it before any network I/O — sheaf.Invalidate dials
	// the daemon and could block long enough to starve SetSheaf /
	// SetCommunityResult writers.
	si.mu.RLock()
	sheaf := si.sheaf
	storedResult := si.result
	si.mu.RUnlock()

	// Use stored membership if caller didn't provide one.
	if membership == nil && storedResult != nil {
		membership = storedResult.Membership
	}

	// If no sheaf backend or no community info, fall back to single invalidation.
	if sheaf == nil || membership == nil {
		si.graph.Invalidate(id)
		return 1
	}

	regionID, ok := membership[id]
	if !ok {
		// Node not in any community — just invalidate it directly.
		si.graph.Invalidate(id)
		return 1
	}

	affected, err := sheaf.Invalidate(regionID)
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
