package leyline

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// SheafSubscriber owns a long-lived `subscribe` connection to the ley-line
// daemon, dispatching pushed `sheaf.invalidate` events to a handler.
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

	// cancel signals the loop to exit; doneCh closes when the loop returns.
	cancel  context.CancelFunc
	doneCh  chan struct{}
	stopMu  sync.Mutex
	stopped bool
}

// EventHandler is called once per pushed sheaf.invalidate event.
// Handler may be invoked concurrently across reconnect boundaries —
// implementations must guard their own state.
type EventHandler func(SheafInvalidateEvent)

// SheafInvalidateEvent is the parsed shape of a sheaf.invalidate event
// pushed by the daemon. Generation is parsed from the wire's quoted-
// string Int64 (capnp-json convention, see PR #382's parseUint64).
type SheafInvalidateEvent struct {
	// Invalidated region IDs the cascade marked stale.
	Invalidated []int
	// Generation is the daemon's monotonic counter at the time of the
	// cascade. Agents compare this to a previously-cached value to
	// decide whether their snapshot is still fresh.
	Generation uint64
	// Count mirrors len(Invalidated); kept as a separate wire field
	// because the daemon emits it that way.
	Count int
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
func (s SubscriberState) String() string {
	switch s {
	case StateConnecting:
		return "connecting"
	case StateConnected:
		return "connected"
	default:
		return "disconnected"
	}
}

// NewSheafSubscriber creates a subscriber that will dial sockPath and
// dispatch events to handler. Use Start to begin the loop; Stop to
// shut it down cleanly.
func NewSheafSubscriber(sockPath string, handler EventHandler) *SheafSubscriber {
	if handler == nil {
		handler = func(SheafInvalidateEvent) {} // no-op
	}
	return &SheafSubscriber{
		sockPath:   sockPath,
		handler:    handler,
		backoffMin: 1 * time.Second,
		backoffMax: 30 * time.Second,
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
// Start is not idempotent — calling it twice on the same subscriber
// spawns two loops competing for the conn. Don't do that.
func (s *SheafSubscriber) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()

	go s.run(ctx)
}

// Stop signals the subscribe loop to terminate and blocks until it has.
// Safe to call multiple times. Always reaches the StateDisconnected
// terminal state before returning.
func (s *SheafSubscriber) Stop() {
	s.stopMu.Lock()
	if s.stopped {
		s.stopMu.Unlock()
		return
	}
	s.stopped = true
	s.stopMu.Unlock()

	s.mu.RLock()
	cancel := s.cancel
	s.mu.RUnlock()
	if cancel != nil {
		cancel()
	}

	// Wait for the loop goroutine to return (or already returned).
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
		if ctx.Err() != nil {
			return
		}

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
		evCh, err := sock.Subscribe([]string{"sheaf.invalidate"})
		if err != nil {
			s.setState(StateDisconnected, fmt.Sprintf("subscribe: %v", err))
			_ = sock.Close()
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff, maxBackoff)
			continue
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

// dispatch parses a single event map and calls the handler. Ignored
// events (wrong topic, malformed) are logged and skipped.
func (s *SheafSubscriber) dispatch(ev map[string]any) {
	topic, _ := ev["topic"].(string)
	if topic != "sheaf.invalidate" {
		// Only sheaf.invalidate is subscribed; defensive ignore.
		return
	}

	parsed := SheafInvalidateEvent{
		Invalidated: parseIntSlice(ev["invalidated"]),
		Generation:  parseUint64(ev["generation"]),
	}
	if v, ok := ev["count"].(float64); ok {
		parsed.Count = int(v)
	} else {
		parsed.Count = len(parsed.Invalidated)
	}

	s.mu.Lock()
	s.lastEvent = time.Now()
	s.lastGeneration = parsed.Generation
	s.mu.Unlock()

	s.handler(parsed)
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
	if d <= 0 {
		return ctx.Err() == nil
	}
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
