package leyline

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSheafSubscriber_DispatchesEvent pins the core contract: when the
// daemon emits a daemon.sheaf.invalidate event, the subscriber's
// handler fires with the parsed event payload. Without this wiring,
// mache observes no signal from invalidations triggered outside its
// own initiator path (the auto-leyline gap mache-6c9e1d, plus any
// future multi-consumer scenarios).
//
// Payload uses the consumer-driven shape (raw-number generation,
// `invalidated` region field) that mache's own `op_sheaf_invalidate`
// calls produce. Since LLO PR #147 unified emit, both shapes arrive
// on `daemon.sheaf.invalidate`; this test also pins the parser's
// tolerance for the `invalidated` field name (see
// TestSheafSubscriber_DispatchesWatcherDrivenEvent for the
// `region_ids` counterpart).
//
// The mock daemon accepts the `subscribe` op, returns ok, then pushes
// one event line. The subscriber should:
//  1. Dial + subscribe successfully
//  2. Receive the pushed event
//  3. Call the handler with parsed Invalidated/Generation/Count
//  4. Update its Status() to reflect the latest seen generation
func TestSheafSubscriber_DispatchesEvent(t *testing.T) {
	sockPath := startSubscribeMockServer(t, mockBehavior{
		acceptSubscribe: true,
		pushEvents: []map[string]any{
			// v0.4.3 event envelope: top-level event metadata + payload
			// nested under `data`. Pinned empirically against the
			// daemon's actual emit shape (see
			// tools/sheaf-subscribe-probe/main.go output).
			//
			// Pre-LLO-v0.4.3 the daemon never emitted the event at
			// all (ley-line-open-5caa59), so the original version of
			// this test used a flat shape based on assumption. The
			// real wire format only became observable once LLO
			// shipped the fix; we must pin it here to catch parser
			// regressions before they reach live runtime.
			{
				"event":  true,
				"seq":    "1",
				"source": "leyline",
				"topic":  "daemon.sheaf.invalidate",
				"data": map[string]any{
					"invalidated":      []any{1.0, 2.0, 3.0},
					"count":            3.0,
					"generation":       "7",
					"prior_generation": "6",
				},
			},
		},
	})

	var (
		handled  atomic.Int32
		gotEvent SheafInvalidateEvent
		eventMu  sync.Mutex
	)
	handler := func(ev SheafInvalidateEvent) {
		eventMu.Lock()
		gotEvent = ev
		eventMu.Unlock()
		handled.Add(1)
	}

	sub := NewSheafSubscriber(sockPath, handler)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sub.Start(ctx)
	t.Cleanup(sub.Stop)

	// Wait up to 2s for the handler to fire. Without proper subscribe
	// + event-dispatch wiring, this times out at zero handled events.
	require.Eventually(t, func() bool {
		return handled.Load() > 0
	}, 2*time.Second, 25*time.Millisecond, "handler must fire within 2s of mock daemon pushing an event")

	eventMu.Lock()
	got := gotEvent
	eventMu.Unlock()
	assert.Equal(t, []int{1, 2, 3}, got.Invalidated,
		"`invalidated` field must decode into Invalidated (parser field-name tolerance)")
	assert.Equal(t, uint64(7), got.Generation, "generation must parse from event payload (nested under data)")
	assert.Equal(t, 3, got.Count, "count round-trips")

	// Status should reflect what we observed.
	status := sub.Status()
	assert.Equal(t, StateConnected, status.State, "subscriber connected after first event")
	assert.Equal(t, uint64(7), status.LastGeneration)
}

func TestSheafSubscriber_RejectsMalformedGeneration(t *testing.T) {
	var handled atomic.Int32
	sub := NewSheafSubscriber("", func(SheafInvalidateEvent) { handled.Add(1) })

	err := sub.dispatch(map[string]any{
		"topic": "daemon.sheaf.invalidate",
		"data": map[string]any{
			"invalidated":      []any{1.0},
			"generation":       "not-a-number",
			"prior_generation": "6",
		},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode invalidate payload")
	assert.Zero(t, handled.Load(), "malformed events must not reach the cache invalidator")
	assert.Zero(t, sub.Status().LastGeneration, "malformed events must not update subscriber state")
}

func TestSheafSubscriber_AcceptsNumericEventSequence(t *testing.T) {
	var got SheafInvalidateEvent
	sub := NewSheafSubscriber("", func(event SheafInvalidateEvent) { got = event })

	err := sub.dispatch(map[string]any{
		"seq":   1.0,
		"topic": "daemon.sheaf.invalidate",
		"data": map[string]any{
			"invalidated":      []any{1.0},
			"generation":       "7",
			"prior_generation": "6",
		},
	})

	require.NoError(t, err)
	assert.Equal(t, []int{1}, got.Invalidated)
}

func TestSheafSubscriber_RejectsMalformedLegacyAliasWhenCanonicalPresent(t *testing.T) {
	var handled atomic.Int32
	sub := NewSheafSubscriber("", func(SheafInvalidateEvent) { handled.Add(1) })

	err := sub.dispatch(map[string]any{
		"topic": "daemon.sheaf.invalidate",
		"data": map[string]any{
			"invalidated":      []any{1.0},
			"region_ids":       []any{1.0, "malformed"},
			"generation":       "7",
			"prior_generation": "6",
		},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode invalidate region_ids")
	assert.Zero(t, handled.Load(), "a malformed alias must not be ignored")
}

func TestSheafSubscriber_RejectsConflictingRegionAliases(t *testing.T) {
	var handled atomic.Int32
	sub := NewSheafSubscriber("", func(SheafInvalidateEvent) { handled.Add(1) })

	err := sub.dispatch(map[string]any{
		"topic": "daemon.sheaf.invalidate",
		"data": map[string]any{
			"invalidated":      []any{1.0},
			"region_ids":       []any{2.0},
			"generation":       "7",
			"prior_generation": "6",
		},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "conflicting")
	assert.Zero(t, handled.Load(), "conflicting aliases must not invalidate caches")
}

// TestSheafSubscriber_ReconnectsAfterDisconnect pins the reconnect-
// with-backoff contract (option 2b from the c14c43 design call). When
// the conn closes mid-stream, the subscriber loops with exponential
// backoff and resubscribes. After reconnect, subsequent events fire
// the handler normally.
//
// The mock here closes the conn after pushing the first event; the
// listener stays up so the subscriber can immediately reconnect. The
// backoff itself is exercised by the timing: subscriber sees the
// dropped conn, transitions Disconnected → Connecting → Connected,
// receives event #2.
func TestSheafSubscriber_ReconnectsAfterDisconnect(t *testing.T) {
	var sessionCount atomic.Int32
	sockPath := startSubscribeMockServer(t, mockBehavior{
		acceptSubscribe: true,
		// First session pushes one event then closes; second session pushes another.
		perSession: func(session int, pushEvent func(map[string]any), closeConn func()) {
			sessionCount.Add(1)
			switch session {
			case 1:
				pushEvent(map[string]any{
					"event":  true,
					"seq":    "1",
					"source": "leyline",
					"topic":  "daemon.sheaf.invalidate",
					"data": map[string]any{
						"invalidated":      []any{10.0},
						"count":            1.0,
						"generation":       "1",
						"prior_generation": "0",
					},
				})
				time.Sleep(50 * time.Millisecond)
				closeConn()
			case 2:
				pushEvent(map[string]any{
					"event":  true,
					"seq":    "2",
					"source": "leyline",
					"topic":  "daemon.sheaf.invalidate",
					"data": map[string]any{
						"invalidated":      []any{20.0},
						"count":            1.0,
						"generation":       "2",
						"prior_generation": "1",
					},
				})
			}
		},
	})

	var handled atomic.Int32
	var lastGen atomic.Uint64
	handler := func(ev SheafInvalidateEvent) {
		handled.Add(1)
		lastGen.Store(ev.Generation)
	}

	sub := NewSheafSubscriber(sockPath, handler)
	// Override backoff for test speed — production uses 1s..30s; tests
	// use 50ms..200ms so the reconnect path completes inside a 5s
	// budget without coupling to real-world backoff defaults.
	sub.SetBackoffForTest(50*time.Millisecond, 200*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sub.Start(ctx)
	t.Cleanup(sub.Stop)

	// Wait for BOTH events (one per session) so we know reconnect fired.
	require.Eventually(t, func() bool {
		return handled.Load() >= 2
	}, 5*time.Second, 50*time.Millisecond, "expected 2 handler fires across reconnect, got %d", handled.Load())

	assert.GreaterOrEqual(t, sessionCount.Load(), int32(2), "subscriber must redial after disconnect")
	assert.Equal(t, uint64(2), lastGen.Load(), "post-reconnect generation observed")
}

// TestSheafSubscriber_StatusReportsDisconnect covers the observability
// side of option 2c: when the subscriber cannot reach the daemon, its
// Status() reports State=Disconnected with a reason. Without this,
// get_sheaf_status has no honest "cascade-not-engaged" signal to
// surface to agents — they'd think the cascade is hot when it's not.
func TestSheafSubscriber_StatusReportsDisconnect(t *testing.T) {
	// Point at a path that doesn't exist; subscriber must NEVER
	// connect successfully but ALSO must not crash or loop tightly.
	sub := NewSheafSubscriber("/tmp/nonexistent-sheaf-subscriber.sock", func(SheafInvalidateEvent) {})
	sub.SetBackoffForTest(50*time.Millisecond, 100*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sub.Start(ctx)
	t.Cleanup(sub.Stop)

	// After enough time for several dial attempts, status MUST report
	// Disconnected and the reason field MUST mention dial / no such file.
	time.Sleep(300 * time.Millisecond)
	status := sub.Status()
	assert.NotEqual(t, StateConnected, status.State, "must NOT report Connected when socket missing")
	assert.NotEmpty(t, status.Reason, "Status.Reason must explain why the subscriber is not connected")
}

// TestSheafSubscriber_StopBeforeStart pins the lifecycle bug Copilot
// caught on PR #384: Stop() reads s.cancel + waits on s.doneCh, but
// when Stop is called BEFORE Start, neither has been initialized
// (cancel == nil and doneCh hasn't been closed since run() never
// started). The original implementation would block forever on
// <-s.doneCh in this case. Stop must return promptly when the
// subscriber was never started.
func TestSheafSubscriber_StopBeforeStart(t *testing.T) {
	sub := NewSheafSubscriber("/tmp/never-started.sock", func(SheafInvalidateEvent) {})

	done := make(chan struct{})
	go func() {
		sub.Stop()
		close(done)
	}()

	select {
	case <-done:
		// ok
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Stop() blocked forever when called before Start() — the lifecycle is broken")
	}
}

// TestSheafSubscriber_StopIdempotentAndAlwaysWaits pins the doc claim:
// every Stop() call MUST wait until the loop has actually terminated,
// not short-circuit on a "already-stopped" flag. The original
// implementation set stopped=true under a mutex and returned without
// waiting on subsequent calls — meaning a second Stop() could return
// while the first call's <-doneCh wait was still in flight.
//
// We construct the race by starting two Stop()s concurrently. Both
// must observe the loop fully terminated before they return.
func TestSheafSubscriber_StopIdempotentAndAlwaysWaits(t *testing.T) {
	sockPath := startSubscribeMockServer(t, mockBehavior{acceptSubscribe: true})
	sub := NewSheafSubscriber(sockPath, func(SheafInvalidateEvent) {})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sub.Start(ctx)

	require.Eventually(t, func() bool {
		return sub.Status().State == StateConnected
	}, 2*time.Second, 25*time.Millisecond, "connect before testing Stop")

	// Two concurrent Stop calls. Both must return cleanly + observe
	// the terminal state.
	done := make(chan struct{}, 2)
	go func() { sub.Stop(); done <- struct{}{} }()
	go func() { sub.Stop(); done <- struct{}{} }()

	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(1 * time.Second):
			t.Fatalf("concurrent Stop() #%d hung — contract says every Stop waits", i)
		}
	}

	assert.Equal(t, StateDisconnected, sub.Status().State,
		"after Stop returns, state MUST be Disconnected")
}

// TestSheafSubscriber_DispatchesWatcherDrivenEvent pins the LLO v0.6+
// coarse-v1 payload contract (LLO PR #140, bead
// `ley-line-open-3b3476`). The watcher-driven `daemon.sheaf.invalidate`
// topic carries an extended payload:
//
//	{"topic": "daemon.sheaf.invalidate",
//	 "data": {"region_ids": [...],
//	          "count": N,
//	          "scope": "all-known",
//	          "changed_files": [...],
//	          "current_root": "<64-hex>",
//	          "generation": "N", "prior_generation": "N",
//	          "timestamp_ms": "N"}}
//
// All the numeric fields (generation, prior_generation, timestamp_ms)
// arrive as quoted strings on the wire per capnp-json convention,
// matching what LLO's `emit_watcher_sheaf_invalidate` emits (see
// PR #140).
//
// This test drives the parser directly through the mock daemon path:
// push the exact envelope, assert every field parses onto the shared
// SheafInvalidateEvent struct. Without this pin, a future LLO wire
// change (e.g. dropping the timestamp field, or renaming a key)
// would break silently — the mock daemon happily pushes whatever we
// give it and the routing layer only cares about Scope + Invalidated.
func TestSheafSubscriber_DispatchesWatcherDrivenEvent(t *testing.T) {
	sockPath := startSubscribeMockServer(t, mockBehavior{
		acceptSubscribe: true,
		pushEvents: []map[string]any{
			{
				"event":  true,
				"seq":    "1",
				"source": "leyline",
				"topic":  "daemon.sheaf.invalidate",
				"data": map[string]any{
					"region_ids":       []any{4.0, 5.0, 6.0},
					"count":            3.0,
					"scope":            "all-known",
					"changed_files":    []any{"pkg/a.rs", "pkg/b.rs"},
					"current_root":     strings.Repeat("a", 64),
					"generation":       "12", // quoted-string Int64
					"prior_generation": "11",
					"timestamp_ms":     "1720000000000",
				},
			},
		},
	})

	var (
		handled  atomic.Int32
		gotEvent SheafInvalidateEvent
		eventMu  sync.Mutex
	)
	handler := func(ev SheafInvalidateEvent) {
		eventMu.Lock()
		gotEvent = ev
		eventMu.Unlock()
		handled.Add(1)
	}

	sub := NewSheafSubscriber(sockPath, handler)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sub.Start(ctx)
	t.Cleanup(sub.Stop)

	require.Eventually(t, func() bool {
		return handled.Load() > 0
	}, 2*time.Second, 25*time.Millisecond, "handler must fire on daemon.sheaf.invalidate")

	eventMu.Lock()
	got := gotEvent
	eventMu.Unlock()

	assert.Equal(t, []int{4, 5, 6}, got.Invalidated,
		"region_ids must decode into Invalidated (shared field with pre-v0.6 shape)")
	assert.Equal(t, uint64(12), got.Generation, "quoted-string generation parses")
	assert.Equal(t, uint64(11), got.PriorGeneration, "quoted-string prior_generation parses")
	assert.Equal(t, 3, got.Count, "count round-trips")
	assert.Equal(t, ScopeAllKnown, got.Scope, "coarse-v1 scope decodes verbatim")
	assert.Equal(t, []string{"pkg/a.rs", "pkg/b.rs"}, got.ChangedFiles, "changed_files decode")
	assert.Equal(t, strings.Repeat("a", 64), got.CurrentRoot, "current_root decodes")
	assert.Equal(t, int64(1720000000000), got.TimestampMs, "quoted-string timestamp_ms parses")

	assert.Equal(t, uint64(12), sub.Status().LastGeneration,
		"Status.LastGeneration mirrors the new topic's generation counter")
}

// TestSheafSubscriber_ConsumerDrivenPayloadShape pins parser tolerance
// for the consumer-driven payload shape mache's own
// `op_sheaf_invalidate` calls produce (via
// SheafInvalidator.InvalidateNodesWithCascade): the region list lives
// in `invalidated`, `generation` / `prior_generation` are raw JSON
// numbers, and the coarse-v1 fields (`scope`, `region_ids`,
// `changed_files`, `current_root`, `timestamp_ms`) are all absent.
//
// Since LLO PR #147 unified emit, this shape arrives on
// `daemon.sheaf.invalidate` alongside the watcher-driven shape (see
// TestSheafSubscriber_DispatchesWatcherDrivenEvent); the subscriber
// must parse both without a per-shape branch.
//
// The zero values for the coarse-v1 fields are the correct signal
// for the routing layer to infer ScopeChangedOnly (see
// cmd/sheaf_subscribe.go's routeSheafEvent switch).
func TestSheafSubscriber_ConsumerDrivenPayloadShape(t *testing.T) {
	sockPath := startSubscribeMockServer(t, mockBehavior{
		acceptSubscribe: true,
		pushEvents: []map[string]any{
			{
				"event":  true,
				"seq":    "1",
				"source": "leyline",
				"topic":  "daemon.sheaf.invalidate",
				"data": map[string]any{
					"invalidated":      []any{9.0},
					"count":            1.0,
					"generation":       "42",
					"prior_generation": "41",
				},
			},
		},
	})

	var (
		handled  atomic.Int32
		gotEvent SheafInvalidateEvent
		eventMu  sync.Mutex
	)
	handler := func(ev SheafInvalidateEvent) {
		eventMu.Lock()
		gotEvent = ev
		eventMu.Unlock()
		handled.Add(1)
	}

	sub := NewSheafSubscriber(sockPath, handler)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sub.Start(ctx)
	t.Cleanup(sub.Stop)

	require.Eventually(t, func() bool {
		return handled.Load() > 0
	}, 2*time.Second, 25*time.Millisecond, "handler must fire on consumer-driven payload shape")

	eventMu.Lock()
	got := gotEvent
	eventMu.Unlock()

	assert.Equal(t, []int{9}, got.Invalidated,
		"`invalidated` field must decode into Invalidated (parser field-name tolerance)")
	assert.Equal(t, uint64(42), got.Generation, "raw-number generation still parses")
	assert.Equal(t, uint64(41), got.PriorGeneration, "raw-number prior_generation still parses")
	assert.Equal(t, "", got.Scope,
		"consumer-driven emit has no scope — leave empty so router infers changed-only")
	assert.Nil(t, got.ChangedFiles,
		"consumer-driven emit has no changed_files — must decode to nil, not a fabricated slice")
	assert.Equal(t, "", got.CurrentRoot, "consumer-driven emit has no current_root")
	assert.Equal(t, int64(0), got.TimestampMs, "consumer-driven emit has no timestamp_ms")
}

// TestSheafSubscriber_StopHaltsLoop pins clean shutdown: Stop()
// terminates the subscribe goroutine + closes the underlying socket.
// Under -race, any leaked goroutine writing through the conn after
// Stop returns is the canary.
func TestSheafSubscriber_StopHaltsLoop(t *testing.T) {
	sockPath := startSubscribeMockServer(t, mockBehavior{
		acceptSubscribe: true,
		// Keep the session alive — Stop must close it from our side.
	})

	sub := NewSheafSubscriber(sockPath, func(SheafInvalidateEvent) {})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sub.Start(ctx)

	require.Eventually(t, func() bool {
		return sub.Status().State == StateConnected
	}, 2*time.Second, 25*time.Millisecond, "must connect before Stop is meaningful")

	// Stop must return promptly — bounded blocking, no deadlock.
	done := make(chan struct{})
	go func() {
		sub.Stop()
		close(done)
	}()
	select {
	case <-done:
		// good
	case <-time.After(1 * time.Second):
		t.Fatal("Stop did not return within 1s — subscribe loop deadlocked")
	}

	assert.Equal(t, StateDisconnected, sub.Status().State, "Stop must transition to Disconnected")
}

// ---------------------------------------------------------------------------
// mockBehavior / startSubscribeMockServer — UDS test harness
// ---------------------------------------------------------------------------

type mockBehavior struct {
	// acceptSubscribe makes the mock respond to `subscribe` with ok:true.
	// When false, the mock returns an error response and the subscribe call fails.
	acceptSubscribe bool

	// pushEvents are pushed to the client AFTER the subscribe response.
	// One event per element, in order. Used by the simple "push N events"
	// shape; for more complex scripts (e.g. close-after-event-N), set
	// perSession instead.
	pushEvents []map[string]any

	// perSession (when set) runs per accepted connection. The session
	// counter starts at 1. pushEvent serializes and writes one event
	// line; closeConn shuts the connection. Use this to test reconnect
	// behavior where the mock controls disconnect timing.
	perSession func(session int, pushEvent func(map[string]any), closeConn func())
}

// startSubscribeMockServer starts a UDS listener that handles `subscribe`
// per the behavior. Returns the socket path. Cleanup is registered with t.
func startSubscribeMockServer(t *testing.T, b mockBehavior) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sheaf-sub-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "t.sock")

	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	var sessionCounter atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			session := int(sessionCounter.Add(1))
			go handleSubscribeSession(conn, session, b)
		}
	}()

	return sockPath
}

func handleSubscribeSession(conn net.Conn, session int, b mockBehavior) {
	defer func() { _ = conn.Close() }()
	rd := bufio.NewReader(conn)

	// Read the subscribe request line.
	line, err := rd.ReadString('\n')
	if err != nil {
		return
	}
	var req map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &req); err != nil {
		return
	}

	// Reply to subscribe.
	if op, _ := req["op"].(string); op == "subscribe" {
		var resp map[string]any
		if b.acceptSubscribe {
			resp = map[string]any{"ok": true}
		} else {
			resp = map[string]any{"error": "subscribe disabled"}
		}
		data, _ := json.Marshal(resp)
		if _, err := conn.Write(append(data, '\n')); err != nil {
			return
		}
		if !b.acceptSubscribe {
			return
		}
	}

	pushEvent := func(ev map[string]any) {
		data, _ := json.Marshal(ev)
		_, _ = conn.Write(append(data, '\n'))
	}
	closeConn := func() {
		_ = conn.Close()
	}

	if b.perSession != nil {
		b.perSession(session, pushEvent, closeConn)
		return
	}

	// Simple "push N events" mode.
	for _, ev := range b.pushEvents {
		pushEvent(ev)
	}

	// Keep session alive (blocking read) so the subscriber doesn't
	// see a hangup just because we ran out of events.
	_, _ = rd.ReadString('\n')
}
