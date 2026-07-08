package cmd

import (
	"context"
	"log"
	"os"
	"path/filepath"
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
	if si == nil { // coverage:ignore — defensive guard; reduction tracked in mache-89b5dd.
		return // coverage:ignore — defensive guard; reduction tracked in mache-89b5dd.
	} // coverage:ignore — defensive guard; reduction tracked in mache-89b5dd.
	r.mu.Lock()
	r.set[si] = struct{}{}
	r.mu.Unlock()
}

// unregister removes si from the routing set. Idempotent.
func (r *sheafEventRouter) unregister(si *graph.SheafInvalidator) {
	if si == nil { // coverage:ignore — defensive guard; reduction tracked in mache-89b5dd.
		return // coverage:ignore — defensive guard; reduction tracked in mache-89b5dd.
	} // coverage:ignore — defensive guard; reduction tracked in mache-89b5dd.
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
func (r *sheafEventRouter) dispatch(ev leyline.SheafInvalidateEvent) { // coverage:ignore — subscriber handler shim; reduction tracked in mache-89b5dd.
	routeSheafEvent(ev, r.snapshot()) // coverage:ignore — subscriber handler shim; reduction tracked in mache-89b5dd.
} // coverage:ignore — subscriber handler shim; reduction tracked in mache-89b5dd.

// routeSheafEvent is the pure routing function. Given an event and a
// slice of invalidators, it dispatches on the event's Scope
// (LLO v0.6+ coarse-v1 payload contract, LLO PR #140):
//
//   - [leyline.ScopeChangedOnly] or empty (pre-v0.6 consumer-driven
//     topic): fine-grained per-region eviction. Each invalidator
//     invalidates the nodes whose community IDs appear in
//     ev.Invalidated. Invalidators with no CommunityResult (or no
//     matching regions) silently no-op. Empty scope maps here because
//     the pre-v0.6 wire didn't carry the field.
//
//   - [leyline.ScopeAllKnown] (LLO v0.6+ watcher-driven coarse-v1):
//     evict every sheaf-scoped cache entry each invalidator holds.
//     The daemon has told us its whole working set is stale;
//     re-cascading region-by-region against the daemon would be
//     redundant chatter, so we invalidate locally via
//     [graph.SheafInvalidator.InvalidateAllScoped].
//
//   - Any other non-empty Scope: log a warning and treat as
//     ScopeAllKnown. Over-invalidating is always safe; the failure
//     mode we refuse is silently serving stale data because we
//     didn't recognise a new sentinel.
//
// If NO invalidator claims any region under changed-only, the event
// is logged + skipped per the c14c43 contract (question 4) — we do
// not buffer events waiting for a CommunityResult to appear. The
// cascade math the event reports is only meaningful relative to the
// topology that produced it; by the time a future get_communities
// install runs, the topology will have been re-pushed and the daemon
// will re-cascade from there. The all-known path skips this guard
// because it invalidates via membership, not via specific IDs.
func routeSheafEvent(ev leyline.SheafInvalidateEvent, invalidators []*graph.SheafInvalidator) {
	switch ev.Scope {
	case "", leyline.ScopeChangedOnly:
		routeSheafEventChangedOnly(ev, invalidators)
	case leyline.ScopeAllKnown:
		routeSheafEventAllKnown(ev, invalidators)
	default:
		// Unknown scope — future LLO release added a value we don't
		// know. Log at the level agents will actually see, then fall
		// through to the safest behavior. This is the SAFE-DEFAULT
		// gate; if we ever add a scope that intentionally requires
		// less-than-all invalidation, do NOT reuse ScopeAllKnown as
		// its fallback here — add an explicit case for it.
		log.Printf("sheaf event: unknown scope %q (generation=%d count=%d); treating as %q (over-invalidating rather than serve stale)",
			ev.Scope, ev.Generation, ev.Count, leyline.ScopeAllKnown)
		routeSheafEventAllKnown(ev, invalidators)
	}
}

// routeSheafEventChangedOnly is the fine-grained per-region path.
// Extracted from the prior routeSheafEvent body verbatim so the
// dispatcher above stays a pure switch.
func routeSheafEventChangedOnly(ev leyline.SheafInvalidateEvent, invalidators []*graph.SheafInvalidator) {
	if len(ev.Invalidated) == 0 { // coverage:ignore — defensive guard; reduction tracked in mache-89b5dd.
		return // coverage:ignore — defensive guard; reduction tracked in mache-89b5dd.
	} // coverage:ignore — defensive guard; reduction tracked in mache-89b5dd.

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

// routeSheafEventAllKnown is the coarse-v1 "invalidate everything"
// path. For each invalidator with an installed CommunityResult, evict
// every node in its Membership. Invalidators without a result no-op
// (nothing to invalidate — the cascade for that window is unreachable
// until a get_communities install runs).
//
// We deliberately do NOT emit the "no invalidator matched" log the
// changed-only path emits: coarse-v1 events fire on every watcher
// cycle regardless of whether mache has a result yet, and an early
// serve session where get_communities hasn't run would spam the log.
func routeSheafEventAllKnown(ev leyline.SheafInvalidateEvent, invalidators []*graph.SheafInvalidator) {
	for _, si := range invalidators {
		if !si.HasResult() {
			continue
		}
		si.InvalidateAllScoped()
	}
}

// siInvalidateRegions converts a list of region IDs into the union of
// node IDs that belong to them (per the invalidator's membership), then
// invalidates each via InvalidateNodesWithCascade. Returns true if at
// least one node was invalidated — used by the router to detect
// "nobody claimed this event" vs. "claimed but empty member set".
func siInvalidateRegions(si *graph.SheafInvalidator, regions []int) bool {
	cr := si.CommunityResultForRouting()
	if cr == nil { // coverage:ignore — defensive guard; reduction tracked in mache-89b5dd.
		return false // coverage:ignore — defensive guard; reduction tracked in mache-89b5dd.
	} // coverage:ignore — defensive guard; reduction tracked in mache-89b5dd.

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

// startSheafSubscriber starts a subscriber that routes daemon-pushed
// events through r. ALWAYS starts the subscriber, regardless of
// whether a daemon socket exists at startup — the subscriber's
// reconnect-with-backoff loop handles initial absence, so any later
// auto-spawn (get_communities, LSP enrichment) brings up a daemon
// that the subscriber will pick up on its next dial attempt.
//
// Returning the subscriber pointer lets the MCP get_sheaf_status
// handler poll its Status() to surface subscriber state to agents.
// When no path could be resolved (no LEYLINE_SOCKET and no usable
// home dir), returns nil + a no-op stop — but in practice the home-
// dir fallback path is always derivable, so the nil case is a true
// "this environment is broken in a way the subscriber can't paper
// over" signal.
//
// Lives in cmd/ rather than internal/leyline so the routing closure
// can reference the router (which holds *graph.SheafInvalidator) — a
// type that internal/leyline can't import without a cycle.
//
// Fixed in response to PR #384 Copilot #3: the prior implementation
// called DiscoverSocket up front, so any auto-spawn that brought a
// daemon up later silently never had a subscriber attached.
func startSheafSubscriber(ctx context.Context, r *sheafEventRouter) (sub *leyline.SheafSubscriber, stop func()) {
	sockPath := resolveSubscriberSocketPath()
	if sockPath == "" {
		log.Printf("sheaf subscriber: cannot resolve a deterministic socket path (LEYLINE_SOCKET unset, no usable home dir); cascade events will not be observed") // coverage:ignore — defensive fail-loud branch; reduction tracked in mache-89b5dd.
		return nil, func() {}                                                                                                                                      // coverage:ignore — defensive fail-loud branch; reduction tracked in mache-89b5dd.
	}

	sub = leyline.NewSheafSubscriber(sockPath, r.dispatch) // coverage:ignore — serve startup wiring; reduction tracked in mache-89b5dd.
	sub.Start(ctx)                                         // coverage:ignore — serve startup wiring; reduction tracked in mache-89b5dd.
	return sub, sub.Stop                                   // coverage:ignore — serve startup wiring; reduction tracked in mache-89b5dd.
}

// resolveSubscriberSocketPath picks the path the subscriber should
// dial. Doesn't check whether the file exists — the subscriber's
// retry loop handles absence. Order:
//  1. $LEYLINE_SOCKET if set (env override; matches what
//     DiscoverSocket would resolve to in the same call).
//  2. ~/.mache/default.sock — the well-known kiln deployment path
//     that auto-spawned daemons use too. Lets the subscriber pick
//     up a daemon a later DiscoverOrStart spawns without restart.
//
// Returns "" when no home dir is determinable. Real-world systems
// always have one; the empty case is a fail-loud signal rather than
// a usable fallback.
func resolveSubscriberSocketPath() string {
	if env := os.Getenv("LEYLINE_SOCKET"); env != "" {
		return env
	}
	home, err := os.UserHomeDir() // coverage:ignore — defensive home-dir resolution; reduction tracked in mache-89b5dd.
	if err != nil {               // coverage:ignore — defensive guard; reduction tracked in mache-89b5dd.
		return "" // coverage:ignore — defensive guard; reduction tracked in mache-89b5dd.
	} // coverage:ignore — defensive guard; reduction tracked in mache-89b5dd.
	return filepath.Join(home, ".mache", "default.sock") // coverage:ignore — fallback path resolution; reduction tracked in mache-89b5dd.
}
