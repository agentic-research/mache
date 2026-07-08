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
//
// Returns the prior backend so callers that own SheafBackend resources
// (e.g. a SheafClient wrapping a UDS socket) can close the prior one
// without leaking. Returns nil if no prior backend was installed.
func (si *SheafInvalidator) SetSheaf(b SheafBackend) (prior SheafBackend) {
	si.mu.Lock()
	defer si.mu.Unlock()
	prior = si.sheaf
	si.sheaf = b
	return prior
}

// SetState atomically swaps both the CommunityResult and SheafBackend
// under a single Lock. Use this when both pieces are being replaced
// together (e.g. from get_communities after PushTopology succeeds).
// The standalone SetSheaf / SetCommunityResult paths open a window
// where the watcher can observe new membership paired with old sheaf
// (or vice versa) — see TestSheafInvalidator_SetState_AtomicSwap and
// PR #383 Copilot #6 for the cascade-mismatch failure mode.
//
// Returns the prior backend so the caller can close it without
// leaking. Returns nil if no prior backend was installed.
func (si *SheafInvalidator) SetState(result *CommunityResult, sheaf SheafBackend) (prior SheafBackend) {
	si.mu.Lock()
	defer si.mu.Unlock()
	prior = si.sheaf
	si.result = result
	si.sheaf = sheaf
	return prior
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

// CommunityResultForRouting returns the current CommunityResult under a
// read lock. The pointer is returned without cloning — callers MUST
// treat it as read-only (the membership map is replaced wholesale by
// future SetCommunityResult / SetState writers, which install a NEW
// pointer rather than mutating in place, so a stable snapshot is safe
// to iterate without further locking).
//
// Exists for the sheaf event subscriber's router (cmd/sheaf_subscribe.go),
// which needs to walk membership to convert region IDs into node IDs
// for invalidation. Named "ForRouting" rather than the obvious
// CommunityResult() to signal the read-only contract.
func (si *SheafInvalidator) CommunityResultForRouting() *CommunityResult { // coverage:ignore — read-only accessor; reduction tracked in mache-89b5dd.
	if si == nil { // coverage:ignore — defensive guard; reduction tracked in mache-89b5dd.
		return nil // coverage:ignore — defensive guard; reduction tracked in mache-89b5dd.
	} // coverage:ignore — defensive guard; reduction tracked in mache-89b5dd.
	si.mu.RLock()         // coverage:ignore — read-only accessor; reduction tracked in mache-89b5dd.
	defer si.mu.RUnlock() // coverage:ignore — read-only accessor; reduction tracked in mache-89b5dd.
	return si.result      // coverage:ignore — read-only accessor; reduction tracked in mache-89b5dd.
}

// InvalidateAllScoped evicts every node the invalidator's
// CommunityResult claims membership for, WITHOUT calling the sheaf
// backend to compute a cascade. Returns the number of nodes evicted.
//
// This is the coarse-v1 "invalidate everything" path used by the
// watcher-driven `daemon.sheaf.invalidate` topic (LLO PR #140, LLO
// v0.6+). The daemon has already declared its entire working set
// stale by the time it emits `scope: "all-known"`, so re-cascading
// per-region against the daemon would be redundant chatter — we
// know the answer is "all of them". A future fine-grained
// (`scope: "changed-only"`) emit path exists (LLO bead `e40566`); it
// stays on the per-region cascade path via InvalidateNodesWithCascade.
//
// No-op when the invalidator, graph, or CommunityResult is nil (same
// null-safety contract as InvalidateNodesWithCascade). Skipping the
// backend also means this method does not touch si.sheaf, so it's
// safe to call even when no SheafBackend is installed — the
// membership snapshot is enough.
func (si *SheafInvalidator) InvalidateAllScoped() int {
	if si == nil || si.graph == nil {
		return 0
	}

	// Snapshot Membership under the read lock, then release it before
	// touching the graph — Graph.Invalidate can hold its own lock and
	// we don't want to nest.
	si.mu.RLock()
	storedResult := si.result
	si.mu.RUnlock()

	if storedResult == nil {
		return 0
	}

	invalidated := make(map[string]struct{}, len(storedResult.Membership))
	for nodeID := range storedResult.Membership {
		if _, seen := invalidated[nodeID]; seen {
			continue
		}
		si.graph.Invalidate(nodeID)
		invalidated[nodeID] = struct{}{}
	}
	return len(invalidated)
}

// InvalidateNodesWithCascade is the batched-by-region variant of
// InvalidateWithCascade. The watcher passes every node touched by a
// file edit; this method dedupes them to the underlying set of unique
// region IDs and calls the daemon exactly once per unique region.
//
// Why this exists (PR #383 Copilot #7): the naive
// `for _, id := range ids { InvalidateWithCascade(id) }` loop hits the
// daemon once per node. On a large file with 50 functions all in the
// same community, that's 50 redundant region invalidations — each
// bumps the generation counter and re-cascades the same regions on
// the daemon side. This method collapses that to one call per unique
// region.
//
// IDs not present in membership fall back to single-node
// Graph.Invalidate (same fallback as InvalidateWithCascade for unknown
// nodes). When sheaf or membership is nil, the entire input falls
// back to per-id Graph.Invalidate.
//
// Returns total nodes invalidated across all paths (cascades + fallbacks).
func (si *SheafInvalidator) InvalidateNodesWithCascade(ids []string) int {
	if si == nil || si.graph == nil || len(ids) == 0 {
		return 0
	}

	si.mu.RLock()
	sheaf := si.sheaf
	storedResult := si.result
	si.mu.RUnlock()

	var membership map[string]int
	if storedResult != nil {
		membership = storedResult.Membership
	}

	// Degraded path: no sheaf or no membership → per-id single-node invalidate.
	if sheaf == nil || membership == nil {
		for _, id := range ids {
			si.graph.Invalidate(id)
		}
		return len(ids)
	}

	// Split inputs: nodes that map to a region (eligible for cascade)
	// vs. unmapped (fall back to single-node invalidate).
	regionSet := make(map[int]struct{})
	var unmapped []string
	for _, id := range ids {
		if rid, ok := membership[id]; ok {
			regionSet[rid] = struct{}{}
		} else {
			unmapped = append(unmapped, id)
		}
	}

	// If nothing maps to a region, fall back per-id.
	if len(regionSet) == 0 {
		for _, id := range ids {
			si.graph.Invalidate(id)
		}
		return len(ids)
	}

	// Call the daemon ONCE per unique region; union the affected sets.
	affectedSet := make(map[int]struct{})
	for region := range regionSet {
		affected, err := sheaf.Invalidate(region)
		if err != nil {
			log.Printf("sheaf invalidate region %d: %v (falling back to single node)", region, err)
			// Best-effort: invalidate the requesting region's member
			// nodes via the local graph so this region isn't entirely
			// dropped, then continue with the remaining regions.
			for nodeID, cid := range membership {
				if cid == region {
					si.graph.Invalidate(nodeID)
				}
			}
			continue
		}
		for _, rid := range affected {
			affectedSet[rid] = struct{}{}
		}
		// Daemon-returned cascade may be empty (no entries to
		// invalidate); the input region itself is still stale, so
		// include it for the local invalidation pass below.
		affectedSet[region] = struct{}{}
	}

	// Invalidate every node in the union of affected regions exactly once.
	invalidated := make(map[string]struct{})
	for nodeID, cid := range membership {
		if _, hit := affectedSet[cid]; hit {
			if _, seen := invalidated[nodeID]; !seen {
				si.graph.Invalidate(nodeID)
				invalidated[nodeID] = struct{}{}
			}
		}
	}

	// Plus unmapped inputs (renames / new constructs not yet in the
	// stored CommunityResult) — fall back to single-node invalidate.
	for _, id := range unmapped {
		if _, seen := invalidated[id]; !seen {
			si.graph.Invalidate(id)
			invalidated[id] = struct{}{}
		}
	}

	return len(invalidated)
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
