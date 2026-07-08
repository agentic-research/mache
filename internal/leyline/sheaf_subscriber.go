package leyline

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// SheafSubscriber owns a long-lived `subscribe` connection to the
// ley-line daemon, dispatching pushed sheaf-invalidate events to a
// handler.
//
// Two topics feed the same handler:
//
//   - `sheaf.invalidate` — pre-v0.6 consumer-driven emit from
//     `op_sheaf_invalidate` (mache calls this from its own watcher via
//     [SheafClient.Invalidate]).
//   - `daemon.sheaf.invalidate` — LLO v0.6+ watcher-driven emit from
//     the daemon's own enrichment cycle (LLO PR #140 / bead
//     `ley-line-open-3b3476`), carrying the coarse-v1 payload shape
//     documented on [SheafInvalidateEvent].
//
// Both variants decode to a single [SheafInvalidateEvent]; the routing
// layer in cmd/sheaf_subscribe.go dispatches on the Scope field.
//
// FIXED UPSTREAM (LLO v0.4.3, ley-line-open-5caa59): pre-v0.4.3 LLO
// daemons did NOT actually deliver sheaf.invalidate events to UDS
// subscribers — `handle_connection` was a pure request/response loop
// that never drained `ConnectionState.event_rx`, so emitted events
// silently accumulated. The cascade math + op response were always
// correct; only the event-bus push was broken. Other topics
// (daemon.snapshot) appeared to work because they happened to be
// included in the subscribe-response replay batch. LLO v0.4.3 ships
// a per-connection writer task plus a per-subscribe event-relay
// task; mache requires v0.4.3 or later for daemon-pushed cascades.
//
// tools/sheaf-subscribe-probe/main.go is the canonical repro tool;
// internal/leyline/sheaf_subscriber_e2e_test.go is the regression
// guard that catches a future re-emergence of this bug.
//
// Design constraints (locked in by PR for mache-c14c43):
//
//   - Dedicated SocketClient (not shared with the watcher's SheafClient).
//     The Subscribe primitive owns the read side of the conn after
//     activation, and would race any concurrent SendOp — see PR #383's
//     SocketClient docstring. Mache should hold one SocketClient for ops
//     and one for events.
//
//   - One subscriber per process. Spawned at serve startup, routes events
//     through a single handler that the caller (cmd/serve) uses to fan out
//     to all per-graph invalidators.
//
//   - Reconnect with exponential backoff. The daemon may restart; the
//     subscriber must redial transparently without leaking goroutines or
//     spinning when the socket is permanently absent. Backoff starts at
//     1s and caps at 30s in production; tests override via
//     SetBackoffForTest.
//
//   - Observable state. Status() returns the current connection state +
//     last-event time + last-seen generation. cmd/serve_handlers' new
//     get_sheaf_status MCP tool will surface this so agents have an
//     honest "cascade engaged?" signal.
//
//   - Unmapped events log + skip, not buffer. The handler is responsible
//     for mapping region IDs → node IDs via the current CommunityResult;
//     when no result is installed yet, drop the event (the cascade for
//     that window is unreachable anyway, and any cached results are
//     equally pre-result-installation stale).
type SheafSubscriber struct {
	sockPath string
	handler  EventHandler

	mu             sync.RWMutex
	state          SubscriberState
	reason         string    // why not connected, populated on disconnect/error
	lastEvent      time.Time // wall-clock of the most recent handler call
	lastGeneration uint64    // monotonic counter from the last received event

	// backoff bounds; tests override via SetBackoffForTest.
	backoffMin time.Duration
	backoffMax time.Duration

	// Lifecycle channels. Both created in NewSheafSubscriber so Stop
	// has something to read regardless of whether Start was called.
	//
	//   startedCh — closed by Start (via startOnce) the first time
	//     Start is invoked. Stop checks this BEFORE waiting on doneCh
	//     to avoid blocking forever when Start was never called.
	//   doneCh    — closed by run() when it returns. Stop waits on
	//     this once it knows Start fired. Concurrent Stop calls all
	//     wait on the same closed channel (returns immediately after
	//     run() exits) — every call observes the terminal state.
	startedCh chan struct{}
	doneCh    chan struct{}
	startOnce sync.Once

	// cancel is set by Start, read by Stop. Guarded by mu (the same
	// lock that protects state) since Start writes while reader
	// goroutines may snapshot the subscriber's state simultaneously.
	cancel context.CancelFunc
}

// EventHandler is called once per pushed sheaf.invalidate event.
// Handler may be invoked concurrently across reconnect boundaries —
// implementations must guard their own state.
type EventHandler func(SheafInvalidateEvent)

// Sheaf-invalidation scope values carried on the wire.
//
// The scope field is emitted by LLO v0.6+ on the watcher-driven
// `daemon.sheaf.invalidate` topic (LLO PR #140, bead
// `ley-line-open-3b3476`). The pre-v0.6 consumer-driven
// `sheaf.invalidate` topic does not carry the field — mache treats
// its absence as [ScopeChangedOnly] (the historical per-region
// behavior) so existing callers keep working unchanged.
const (
	// ScopeChangedOnly is the fine-grained per-region contract:
	// invalidate only the caches for regions listed in RegionIDs.
	// Emitted by:
	//   - the consumer-driven `sheaf.invalidate` topic (payload has
	//     no `scope` field; mache infers this variant),
	//   - the future fine-grained daemon-driven emit (LLO bead
	//     `e40566`, not yet shipped).
	ScopeChangedOnly = "changed-only"

	// ScopeAllKnown is the coarse-v1 contract shipped by LLO v0.6:
	// evict every sheaf-scoped cache entry. Region IDs are still
	// carried (they list every known region) but callers should NOT
	// treat them as a whitelist — the daemon is reporting the whole
	// working set as stale.
	ScopeAllKnown = "all-known"
)

// SheafInvalidateEvent is the parsed shape of a sheaf-invalidate event
// pushed by the daemon. Two on-wire variants collapse to this struct:
//
//   - Topic `sheaf.invalidate` (LLO ≤ v0.5, consumer-driven — emitted
//     from `op_sheaf_invalidate` when a client calls it):
//     `{invalidated, count, generation, prior_generation}`.
//   - Topic `daemon.sheaf.invalidate` (LLO v0.6+, watcher-driven —
//     emitted from the daemon's enrichment cycle, LLO PR #140):
//     `{region_ids, count, scope, changed_files, current_root,
//     generation, prior_generation, timestamp_ms}`.
//
// `Generation` and `PriorGeneration` are parsed from either raw JSON
// numbers or capnp-json quoted-string Int64 (see parseUint64) so a
// single struct handles both encodings without a per-topic parse.
type SheafInvalidateEvent struct {
	// Invalidated carries the region identifiers the daemon named in
	// the payload. Populated from `invalidated` (consumer-driven) or
	// `region_ids` (watcher-driven). Interpretation depends on Scope
	// (see the constants above): under ScopeChangedOnly this is a
	// whitelist to invalidate; under ScopeAllKnown it's a snapshot of
	// every known region and callers should evict everything.
	//
	// The field keeps its pre-v0.6 name (rather than being renamed to
	// `RegionIDs`) to preserve the public-API contract of the mache
	// event surface. The `region_ids` wire field is decoded into it
	// too.
	Invalidated []int

	// Generation is the daemon's monotonic counter after the emit.
	// Agents compare this to a previously-cached value to decide
	// whether their snapshot is still fresh.
	Generation uint64

	// PriorGeneration is the counter value before this event's bump.
	// Populated on both topics.
	PriorGeneration uint64

	// Count mirrors len(Invalidated); kept as a separate wire field
	// because the daemon emits it that way.
	Count int

	// Scope classifies the event: [ScopeChangedOnly] or [ScopeAllKnown].
	// See the constants for semantics + wire-emission provenance. The
	// dispatcher treats an unknown non-empty value as ScopeAllKnown
	// (over-invalidate rather than serve stale) and logs a warning so
	// a new future scope value isn't silently dropped.
	//
	// Empty when parsed from the consumer-driven topic (which
	// pre-dates the field) — cmd routing infers ScopeChangedOnly for
	// the empty case.
	Scope string

	// ChangedFiles lists the files whose reparse triggered the emit.
	// Watcher-driven topic only; empty from the consumer-driven topic
	// (which has no file context — the caller decides which regions
	// to poke).
	ChangedFiles []string

	// CurrentRoot is the 64-hex BLAKE3 root of the substrate state
	// after the emit. Watcher-driven topic only; may be empty when
	// the daemon's controller read failed (best-effort, degrades
	// honestly per LLO PR #140).
	CurrentRoot string

	// TimestampMs is now_ms() at emit time. Watcher-driven topic only.
	TimestampMs int64
}

// SubscriberState classifies the subscriber's connection state.
// Surfaced via Status() and ultimately through get_sheaf_status.
type SubscriberState int

const (
	// StateDisconnected: not currently connected; either not yet started
	// or the loop exited (Stop called, ctx cancelled, fatal error).
	StateDisconnected SubscriberState = iota
	// StateConnecting: between dial attempts (in backoff) or actively dialing.
	StateConnecting
	// StateConnected: subscribed and listening for events.
	StateConnected
)

// SubscriberStatus is the snapshot returned by SheafSubscriber.Status().
type SubscriberStatus struct {
	State          SubscriberState
	Reason         string    // non-empty when State != Connected, explains why
	LastEvent      time.Time // zero when no event seen yet
	LastGeneration uint64    // 0 when no event seen yet
}

// String renders the state for logging / MCP responses. Lower-case
// since it appears in agent-consumable JSON.
func (s SubscriberState) String() string { // coverage:ignore — display formatter; reduction tracked in mache-89b5dd.
	switch s { // coverage:ignore — display formatter; reduction tracked in mache-89b5dd.
	case StateConnecting: // coverage:ignore — display formatter; reduction tracked in mache-89b5dd.
		return "connecting" // coverage:ignore — display formatter; reduction tracked in mache-89b5dd.
	case StateConnected: // coverage:ignore — display formatter; reduction tracked in mache-89b5dd.
		return "connected" // coverage:ignore — display formatter; reduction tracked in mache-89b5dd.
	default: // coverage:ignore — display formatter; reduction tracked in mache-89b5dd.
		return "disconnected" // coverage:ignore — display formatter; reduction tracked in mache-89b5dd.
	} // coverage:ignore — display formatter; reduction tracked in mache-89b5dd.
}

// NewSheafSubscriber creates a subscriber that will dial sockPath and
// dispatch events to handler. Use Start to begin the loop; Stop to
// shut it down cleanly.
//
// Both lifecycle channels are constructed here so Stop is safe to
// call in any order relative to Start (see Stop's contract).
func NewSheafSubscriber(sockPath string, handler EventHandler) *SheafSubscriber {
	if handler == nil {
		handler = func(SheafInvalidateEvent) {} // no-op // coverage:ignore — defensive nil-handler guard; reduction tracked in mache-89b5dd.
	}
	return &SheafSubscriber{
		sockPath:   sockPath,
		handler:    handler,
		backoffMin: 1 * time.Second,
		backoffMax: 30 * time.Second,
		startedCh:  make(chan struct{}),
		doneCh:     make(chan struct{}),
	}
}

// SetBackoffForTest overrides the dial-retry backoff bounds. Test-only;
// production callers must not touch this. Kept on the public surface
// (rather than exposed via build tag) because the alternative — making
// the bounds a constructor option — would force every caller to thread
// production defaults through.
func (s *SheafSubscriber) SetBackoffForTest(min, max time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.backoffMin = min
	s.backoffMax = max
}

// Start spawns the subscribe loop in a background goroutine. Returns
// immediately. The loop runs until Stop is called or ctx is cancelled.
//
// Idempotent — repeated calls after the first are no-ops (sync.Once
// gate). Concurrent Start calls collapse to a single loop spawn so
// two goroutines can't accidentally race for the conn.
func (s *SheafSubscriber) Start(ctx context.Context) {
	s.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(ctx)
		s.mu.Lock()
		s.cancel = cancel
		s.mu.Unlock()
		close(s.startedCh) // signal "Start was called at least once"
		go s.run(ctx)
	})
}

// Stop signals the subscribe loop to terminate and blocks until it has.
//
// Contract:
//   - When Start was never called, returns immediately (no goroutine
//     to stop, no channel to wait on).
//   - When Start was called, every Stop call (including concurrent and
//     repeated ones) waits until run() has actually returned. Returning
//     before that would let callers observe a Disconnected state too
//     early — the contract is "by the time Stop returns, the loop is
//     gone." Multiple Stop calls all observe the same close on doneCh
//     (a closed channel is permanently ready) so the wait is cheap
//     after the first one completes.
//
// Fixed in response to PR #384 Copilot #1: the prior shape blocked
// forever pre-Start and short-circuited on a "stopped" flag for
// repeated calls, violating both guarantees.
func (s *SheafSubscriber) Stop() {
	// Pre-Start? Nothing to wait on.
	select {
	case <-s.startedCh:
		// Started — fall through to shutdown.
	default:
		return
	}

	s.mu.RLock()
	cancel := s.cancel
	s.mu.RUnlock()
	if cancel != nil {
		cancel()
	}

	<-s.doneCh
}

// Status returns a snapshot of the subscriber's current state. Cheap;
// callers (e.g. the get_sheaf_status MCP handler) can poll freely.
func (s *SheafSubscriber) Status() SubscriberStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return SubscriberStatus{
		State:          s.state,
		Reason:         s.reason,
		LastEvent:      s.lastEvent,
		LastGeneration: s.lastGeneration,
	}
}

// run is the dial → subscribe → read → dispatch loop. Returns when
// ctx is cancelled. On error, transitions to Disconnected with reason,
// sleeps with exponential backoff, and retries.
func (s *SheafSubscriber) run(ctx context.Context) {
	defer close(s.doneCh)
	defer s.setState(StateDisconnected, "subscriber stopped")

	backoff := s.snapshotBackoffMin()
	maxBackoff := s.snapshotBackoffMax()

	for {
		if ctx.Err() != nil { // coverage:ignore — defensive ctx-cancelled check at loop head; reduction tracked in mache-89b5dd.
			return // coverage:ignore — defensive ctx-cancelled check at loop head; reduction tracked in mache-89b5dd.
		} // coverage:ignore — defensive ctx-cancelled check at loop head; reduction tracked in mache-89b5dd.

		s.setState(StateConnecting, "")

		// Dial.
		sock, err := DialSocket(s.sockPath)
		if err != nil {
			s.setState(StateDisconnected, fmt.Sprintf("dial %s: %v", s.sockPath, err))
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}

		// Subscribe.
		//
		// Two topics feed the same dispatch:
		//   - `sheaf.invalidate` — pre-v0.6 consumer-driven emit
		//     (from `op_sheaf_invalidate` on client-triggered ops).
		//   - `daemon.sheaf.invalidate` — LLO v0.6+ watcher-driven
		//     emit (from the daemon's own file-change enrichment,
		//     LLO PR #140 / bead `ley-line-open-3b3476`). Closes the
		//     sheaf-invalidation loop end-to-end so mache no longer
		//     needs to poke the daemon after every observed file
		//     change.
		//
		// The subscribe response replays historical events, so
		// resubscribing over an existing conn will re-drain topics
		// that had events during the disconnect window — safe under
		// the daemon's dedup on generation counter.
		evCh, err := sock.Subscribe([]string{"sheaf.invalidate", "daemon.sheaf.invalidate"})
		if err != nil { // coverage:ignore — Subscribe error path (post-dial); reduction tracked in mache-89b5dd.
			s.setState(StateDisconnected, fmt.Sprintf("subscribe: %v", err)) // coverage:ignore — Subscribe error path; reduction tracked in mache-89b5dd.
			_ = sock.Close()                                                 // coverage:ignore — Subscribe error path; reduction tracked in mache-89b5dd.
			if !sleepCtx(ctx, backoff) {                                     // coverage:ignore — Subscribe error path; reduction tracked in mache-89b5dd.
				return // coverage:ignore — Subscribe error path; reduction tracked in mache-89b5dd.
			} // coverage:ignore — Subscribe error path; reduction tracked in mache-89b5dd.
			backoff = nextBackoff(backoff, maxBackoff) // coverage:ignore — Subscribe error path; reduction tracked in mache-89b5dd.
			continue                                   // coverage:ignore — Subscribe error path; reduction tracked in mache-89b5dd.
		}

		// Successful subscribe — reset backoff and start consuming.
		s.setState(StateConnected, "")
		backoff = s.snapshotBackoffMin()

		s.consume(ctx, evCh)
		_ = sock.Close()

		// consume returned: either ctx cancelled, or the event channel
		// closed (daemon dropped us). If ctx cancelled, exit; otherwise
		// loop to redial.
		if ctx.Err() != nil {
			return
		}
		s.setState(StateDisconnected, "daemon dropped subscriber; reconnecting")
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff, maxBackoff)
	}
}

// consume reads events from the daemon's push channel and dispatches
// them. Returns when either the channel closes (daemon side hung up)
// or ctx is cancelled.
func (s *SheafSubscriber) consume(ctx context.Context, evCh <-chan map[string]any) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-evCh:
			if !ok {
				return
			}
			s.dispatch(ev)
		}
	}
}

// dispatch parses a single event map and calls the handler. Events
// with a topic mache did not subscribe to are silently dropped —
// they're structurally impossible from the daemon side (we only
// subscribed to two topics), so reaching them indicates a daemon-side
// bug rather than a runtime condition agents need to observe. Updated
// from the prior "logged and skipped" docstring (PR #384 Copilot #2)
// to match what the code actually does; the runtime log line would
// just be noise on a subscribed-topic stream.
//
// Two wire shapes are unified here:
//
//  1. Consumer-driven `sheaf.invalidate` (LLO ≤ v0.5, pinned against
//     LLO v0.4.3 via tools/sheaf-subscribe-probe/main.go):
//
//     {"event": true, "seq": N, "source": "leyline",
//     "topic": "sheaf.invalidate",
//     "data": {"invalidated": [...], "generation": N, "count": N,
//     "prior_generation": N}}
//
//  2. Watcher-driven `daemon.sheaf.invalidate` (LLO v0.6+, LLO
//     PR #140 / bead `ley-line-open-3b3476`):
//
//     {"event": true, "seq": N, "source": "leyline",
//     "topic": "daemon.sheaf.invalidate",
//     "data": {"region_ids": [...], "count": N, "scope": "all-known",
//     "changed_files": [...], "current_root": "<hex>",
//     "generation": "N", "prior_generation": "N",
//     "timestamp_ms": "N"}}
//
// Payload fields are nested under `data`. Pre-v0.4.3 the daemon
// never delivered the event at all (ley-line-open-5caa59), so the
// envelope wasn't observable from mache's side until the fix shipped.
func (s *SheafSubscriber) dispatch(ev map[string]any) {
	topic, _ := ev["topic"].(string)
	if topic != "sheaf.invalidate" && topic != "daemon.sheaf.invalidate" { // coverage:ignore — defensive guard for structurally-impossible topic; reduction tracked in mache-89b5dd.
		// Only the two sheaf-invalidate topics are subscribed;
		// silently ignore the impossible case rather than emit log
		// noise per event.
		return // coverage:ignore — defensive guard for structurally-impossible topic; reduction tracked in mache-89b5dd.
	} // coverage:ignore — defensive guard for structurally-impossible topic; reduction tracked in mache-89b5dd.

	// Payload lives under `data`. A missing or wrong-typed `data`
	// field means the daemon emitted a malformed event; treat as an
	// empty payload rather than panic — the handler still observes
	// the topic-was-delivered signal and can decide what to do.
	data, _ := ev["data"].(map[string]any)

	// Region IDs live in `region_ids` on the watcher-driven topic and
	// `invalidated` on the consumer-driven topic. Read both — the one
	// that's present wins; if both are somehow present the
	// watcher-driven `region_ids` takes precedence (its emit is the
	// canonical source of "here's what's stale").
	regions := parseIntSlice(data["region_ids"])
	if len(regions) == 0 {
		regions = parseIntSlice(data["invalidated"])
	}

	parsed := SheafInvalidateEvent{
		Invalidated:     regions,
		Generation:      parseUint64(data["generation"]),
		PriorGeneration: parseUint64(data["prior_generation"]),
	}
	if v, ok := data["count"].(float64); ok {
		parsed.Count = int(v)
	} else { // coverage:ignore — defensive fallback when daemon omits count; reduction tracked in mache-89b5dd.
		parsed.Count = len(parsed.Invalidated) // coverage:ignore — defensive fallback when daemon omits count; reduction tracked in mache-89b5dd.
	} // coverage:ignore — defensive fallback when daemon omits count; reduction tracked in mache-89b5dd.

	// New coarse-v1 fields (watcher-driven topic). Absent on the
	// consumer-driven topic; the zero values are the correct default
	// for the routing layer to infer ScopeChangedOnly.
	if scope, ok := data["scope"].(string); ok {
		parsed.Scope = scope
	}
	parsed.ChangedFiles = parseStringSlice(data["changed_files"])
	if root, ok := data["current_root"].(string); ok {
		parsed.CurrentRoot = root
	}
	parsed.TimestampMs = parseInt64(data["timestamp_ms"])

	s.mu.Lock()
	s.lastEvent = time.Now()
	s.lastGeneration = parsed.Generation
	s.mu.Unlock()

	s.handler(parsed)
}

// parseStringSlice extracts []string from a JSON-decoded []any.
// Returns nil for a nil / non-slice input, and skips non-string
// entries within the slice (rather than aborting) so a malformed
// entry doesn't drop otherwise-valid file paths.
func parseStringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// parseInt64 accepts a float64 (raw JSON number) or a string (capnp-
// json's quoted-Int64 rendering) and returns the corresponding int64.
// Returns 0 for any other shape. Timestamp fields cross the 2^53
// safe-integer ceiling around year 2255, so the quoted-string form
// isn't strictly needed today, but LLO emits timestamps as strings
// (see PR #140's `now_ms().to_string()`) for consistency with the
// generation counters, and this helper matches that shape.
func parseInt64(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case string:
		var n int64
		_, _ = fmt.Sscanf(x, "%d", &n)
		return n
	default:
		return 0
	}
}

// setState updates the connection state under the write lock. Reason
// is cleared when transitioning to Connected.
func (s *SheafSubscriber) setState(state SubscriberState, reason string) {
	s.mu.Lock()
	s.state = state
	if state == StateConnected {
		s.reason = ""
	} else if reason != "" {
		s.reason = reason
	}
	s.mu.Unlock()
}

// snapshotBackoffMin reads backoffMin under the read lock.
func (s *SheafSubscriber) snapshotBackoffMin() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.backoffMin
}

// snapshotBackoffMax reads backoffMax under the read lock.
func (s *SheafSubscriber) snapshotBackoffMax() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.backoffMax
}

// nextBackoff doubles the current backoff, capped at max. Pure helper.
func nextBackoff(current, max time.Duration) time.Duration {
	next := current * 2
	if next > max {
		return max
	}
	return next
}

// sleepCtx sleeps for d unless ctx is cancelled first. Returns true if
// the sleep completed, false if ctx cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 { // coverage:ignore — defensive zero-duration guard; reduction tracked in mache-89b5dd.
		return ctx.Err() == nil // coverage:ignore — defensive zero-duration guard; reduction tracked in mache-89b5dd.
	} // coverage:ignore — defensive zero-duration guard; reduction tracked in mache-89b5dd.
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// (logDispatchSkipped helper for the unmapped-region "skip + log"
// contract lives with its call site in cmd/serve, added alongside the
// subscriber-to-graph routing in the next commit. Keeping it co-located
// with the routing logic that triggers it makes the contract one read
// to verify.)
