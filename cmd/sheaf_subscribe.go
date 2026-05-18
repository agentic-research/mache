package cmd

import (
	"context"
	"log"
	"sync"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/agentic-research/mache/internal/leyline"
)

// sheafEventRouter holds the process-wide registry of SheafInvalidators
// that should receive sheaf.invalidate events from the daemon. When the
// subscriber's handler fires, it walks the snapshot and dispatches to
// every invalidator whose CommunityResult claims at least one of the
// event's region IDs.
//
// Why one router per process: per the c14c43 design call (question 3a),
// mache runs a single ley-line daemon subscription per process. The
// router fans the single event stream out to whichever per-graph
// invalidator owns the affected regions. With one lazyGraph per session
// (the multi-tenant serve mode), this means events route by content
// — whichever graph pushed the matching topology gets the event,
// untouched graphs stay untouched.
//
// Lifecycle: cmd/serve constructs ONE router at startup, hands its
// dispatch closure to a SheafSubscriber, and each lazyGraph registers
// its invalidator on init / unregisters on cleanup. The router itself
// owns no I/O — it's a routing seam under a RWMutex.
type sheafEventRouter struct {
	mu  sync.RWMutex
	set map[*graph.SheafInvalidator]struct{}
}

func newSheafEventRouter() *sheafEventRouter {
	return &sheafEventRouter{set: make(map[*graph.SheafInvalidator]struct{})}
}

// register adds si to the routing set. No-op when si is nil so callers
// (lazyGraph.init) don't have to nil-check before registering — graphs
// without a watcher have nil invalidators by design.
func (r *sheafEventRouter) register(si *graph.SheafInvalidator) {
	if si == nil {
		return
	}
	r.mu.Lock()
	r.set[si] = struct{}{}
	r.mu.Unlock()
}

// unregister removes si from the routing set. Idempotent.
func (r *sheafEventRouter) unregister(si *graph.SheafInvalidator) {
	if si == nil {
		return
	}
	r.mu.Lock()
	delete(r.set, si)
	r.mu.Unlock()
}

// snapshot returns the current set of registered invalidators as a
// slice. Callers iterate the slice without holding the lock so a
// concurrent register/unregister can proceed without starving the
// dispatcher.
func (r *sheafEventRouter) snapshot() []*graph.SheafInvalidator {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*graph.SheafInvalidator, 0, len(r.set))
	for si := range r.set {
		out = append(out, si)
	}
	return out
}

// dispatch is the EventHandler the subscriber calls. Walks the snapshot
// and routes the event to every invalidator whose CommunityResult
// claims at least one of the event's regions.
//
// Kept as a method so the router owns logging + skip semantics.
// Test entry-point routeSheafEvent below takes an explicit slice so
// the dispatch logic is testable without instantiating a router.
func (r *sheafEventRouter) dispatch(ev leyline.SheafInvalidateEvent) {
	routeSheafEvent(ev, r.snapshot())
}

// routeSheafEvent is the pure routing function. Given an event and a
// slice of invalidators, it asks each invalidator to invalidate the
// nodes belonging to any region in ev.Invalidated. Invalidators with
// no CommunityResult (or no matching regions) silently no-op.
//
// If NO invalidator claims any region in the event, the event is
// logged + skipped per the c14c43 contract (question 4) — we do not
// buffer events waiting for a CommunityResult to appear. The cascade
// math the event reports is only meaningful relative to the topology
// that produced it; by the time a future get_communities install
// runs, the topology will have been re-pushed and the daemon will
// re-cascade from there.
func routeSheafEvent(ev leyline.SheafInvalidateEvent, invalidators []*graph.SheafInvalidator) {
	if len(ev.Invalidated) == 0 {
		return
	}

	var matched bool
	for _, si := range invalidators {
		if !si.HasResult() {
			continue
		}
		if siInvalidateRegions(si, ev.Invalidated) {
			matched = true
		}
	}

	if !matched {
		log.Printf("sheaf event skipped (no invalidator claims any of regions %v): generation=%d count=%d",
			ev.Invalidated, ev.Generation, ev.Count)
	}
}

// siInvalidateRegions converts a list of region IDs into the union of
// node IDs that belong to them (per the invalidator's membership), then
// invalidates each via InvalidateNodesWithCascade. Returns true if at
// least one node was invalidated — used by the router to detect
// "nobody claimed this event" vs. "claimed but empty member set".
func siInvalidateRegions(si *graph.SheafInvalidator, regions []int) bool {
	cr := si.CommunityResultForRouting()
	if cr == nil {
		return false
	}

	regionSet := make(map[int]struct{}, len(regions))
	for _, r := range regions {
		regionSet[r] = struct{}{}
	}

	var nodeIDs []string
	for nodeID, cid := range cr.Membership {
		if _, hit := regionSet[cid]; hit {
			nodeIDs = append(nodeIDs, nodeID)
		}
	}

	if len(nodeIDs) == 0 {
		return false
	}

	si.InvalidateNodesWithCascade(nodeIDs)
	return true
}

// startSheafSubscriber dials the daemon socket and starts a subscriber
// that routes events through r. Returns the subscriber (or nil when
// no daemon socket is reachable at startup, in which case the
// returned stop is a no-op and a warning is logged — the watcher path
// still works, just without notifications from other initiators).
//
// Returning the subscriber pointer lets the MCP get_sheaf_status
// handler poll its Status() to surface subscriber state to agents.
//
// Lives in cmd/ rather than internal/leyline so the routing closure
// can reference the router (which holds *graph.SheafInvalidator) — a
// type that internal/leyline can't import without a cycle.
func startSheafSubscriber(ctx context.Context, r *sheafEventRouter) (sub *leyline.SheafSubscriber, stop func()) {
	sockPath, err := leyline.DiscoverSocket()
	if err != nil {
		log.Printf("sheaf subscriber: no daemon socket reachable at startup (%v); cascade events from other initiators will not be observed", err)
		return nil, func() {}
	}

	sub = leyline.NewSheafSubscriber(sockPath, r.dispatch)
	sub.Start(ctx)
	return sub, sub.Stop
}
