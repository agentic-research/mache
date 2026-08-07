package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/leyline"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TDD #2 — buildServeGraph contract: callers receive an invalidator
// alongside the graph, or explicitly nil for modes that don't support
// one (composite mounts, control-only / arena-backed init).
//
// The signature change is deliberate. Hiding the SheafInvalidator
// behind a type assertion on MemoryStore would silently break the day
// someone swaps the backing store — exactly the kind of regression PR
// #380's snapshot fix taught us to lock with an explicit contract.

// TestBuildServeGraph_ReturnsInvalidatorForDir asserts a non-nil
// invalidator comes back for a real source directory — the watcher
// is constructed in this path and needs the invalidator to wire its
// onChange callback.
func TestBuildServeGraph_ReturnsInvalidatorForDir(t *testing.T) {
	t.Setenv("MACHE_NO_LEYLINE", "1")
	dir := writeSourceDir(t, "Foo")

	g, si, cleanup, err := buildServeGraph(dir, &api.Topology{Version: api.SchemaVersion})
	require.NoError(t, err)
	defer cleanup()
	require.NotNil(t, g, "graph must be non-nil")
	require.NotNil(t, si, "invalidator must be non-nil for directory sources")
	assert.False(t, si.HasResult(), "fresh invalidator has no community result until get_communities runs")
}

// TestBuildServeGraph_NilInvalidatorForNonDir asserts the non-dir
// contract: "you get an invalidator iff there's a watcher that can
// fire it" — file-shaped sources never construct a watcher, so they
// never return a non-nil invalidator. Composite mounts and
// control-only init follow the same rule.
//
// Post-ADR-0012-step-4 the contract splits by file kind:
//
//   - A single SOURCE file (.go) is now a HARD ERROR: in-process
//     tree-sitter is gone, the auto-leyline serve path is IsDir-gated,
//     and `leyline parse` rejects non-directory paths — so no parser
//     can serve a bare source file and the engine fails loudly rather
//     than projecting an empty graph. The error path must keep the
//     consistent shape (nil graph, nil invalidator, non-nil noop
//     cleanup) so callers ignoring si don't dangle a partial store.
//   - A single DATA file (.json) still succeeds via the pure-Go JSON
//     walker, and — being a non-dir — still returns nil invalidator,
//     preserving the original intent of this test.
func TestBuildServeGraph_NilInvalidatorForNonDir(t *testing.T) {
	t.Setenv("MACHE_NO_LEYLINE", "1")
	dir := t.TempDir()

	// Source-file shape: hard error, consistent nil/noop shape.
	goFile := filepath.Join(dir, "main.go")
	require.NoError(t, os.WriteFile(goFile, []byte("package main\nfunc Foo() {}\n"), 0o600))

	g, si, cleanup, err := buildServeGraph(goFile, &api.Topology{Version: api.SchemaVersion})
	require.Error(t, err, "single source file has no parser post-CGO-removal — must fail loudly, not project empty")
	assert.Contains(t, err.Error(), "ASTWalker", "error must explain the missing-parser cause")
	assert.Nil(t, g, "no graph on error")
	assert.Nil(t, si, "no invalidator on error")
	require.NotNil(t, cleanup, "cleanup must be non-nil so deferred callers don't nil-deref")
	cleanup() // noop, must not panic

	// Data-file shape: still succeeds, and non-dir ⇒ no watcher ⇒ nil
	// invalidator (the original contract this test pins).
	jsonFile := filepath.Join(dir, "data.json")
	require.NoError(t, os.WriteFile(jsonFile, []byte(`{"name":"x"}`), 0o600))

	g, si, cleanup, err = buildServeGraph(jsonFile, &api.Topology{Version: api.SchemaVersion})
	require.NoError(t, err)
	defer cleanup()
	require.NotNil(t, g)
	assert.Nil(t, si, "single-file source has no watcher → no invalidator")
}

// TestBuildServeGraph_InvalidatorWiredIntoStore asserts that the
// returned invalidator can actually invalidate nodes through the
// graph it was constructed with. This is the smoke test for the
// closure capture inside buildServeGraph — if the invalidator
// references a different graph than the one returned, watcher
// invalidations would silently miss.
func TestBuildServeGraph_InvalidatorWiredIntoStore(t *testing.T) {
	t.Setenv("MACHE_NO_LEYLINE", "1")
	dir := writeSourceDir(t, "Bar")

	g, si, cleanup, err := buildServeGraph(dir, &api.Topology{Version: api.SchemaVersion})
	require.NoError(t, err)
	defer cleanup()
	require.NotNil(t, si)

	// Without a CommunityResult and without a sheaf backend, the
	// invalidator must still successfully call through to the graph's
	// own Invalidate (single-node fallback). Pass an arbitrary node ID
	// — even if the graph has never seen it, Invalidate is a no-op-safe
	// operation on MemoryStore.
	count := si.InvalidateWithCascade("nonexistent/node", nil)
	assert.Equal(t, 1, count, "fallback path: single-node invalidate fires")

	_ = g // graph reference held so the deferred cleanup doesn't race init
}

// TestBuildServeGraph_WatcherFiresInvalidator is the end-to-end
// wiring assertion. It writes a file, lets the watcher debounce, and
// confirms the watcher invoked the invalidator at least once. The
// race-detector run also pins that the watcher goroutine and a
// concurrent SetCommunityResult call don't trip the new mutex
// (covered by TestSheafInvalidator_ConcurrentReadWrite at the unit
// level, this is the integration variant).
// countingSheafBackend records every Invalidate call so tests can
// assert the watcher actually routed through the cascade path. Stand-
// in for the real SheafClient when the test doesn't need (or want) a
// live UDS daemon. Thread-safe — the watcher fires from a goroutine.
type countingSheafBackend struct {
	mu       sync.Mutex
	calls    []int // region IDs received, in call order
	response []int // canned cascade response
}

func (c *countingSheafBackend) Invalidate(regionID int) ([]int, error) {
	c.mu.Lock()
	c.calls = append(c.calls, regionID)
	resp := c.response
	c.mu.Unlock()
	return resp, nil
}

func (c *countingSheafBackend) CallCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func (c *countingSheafBackend) Calls() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]int, len(c.calls))
	copy(out, c.calls)
	return out
}

// TestBuildServeGraph_WatcherFiresInvalidator hardens the prior
// version of this test (PR #383 Copilot #5) — the previous shape
// slept until a deadline and asserted on `si.HasResult()` being
// false, which would have passed even if the watcher→invalidator
// wiring were deleted entirely.
//
// The hardened version installs a counting fake SheafBackend AND a
// synthetic CommunityResult that maps the fixture's node IDs to a
// known region. After editing the file, the test waits up to 2s for
// the fake backend to record at least one Invalidate call. Without
// the watcher→invalidator wiring (or with a regression that bypasses
// the invalidator), the call count stays at 0 and the test fails.
//
// Also pins that the cascade is called with the *correct* region —
// the synthetic membership puts every node from the fixture file
// into region 42, so the only legitimate call recorded is for 42.
func TestBuildServeGraph_WatcherFiresInvalidator(t *testing.T) {
	t.Setenv("MACHE_NO_LEYLINE", "1")
	dir := writeSourceDir(t, "Baz")

	g, si, cleanup, err := buildServeGraph(dir, &api.Topology{Version: api.SchemaVersion})
	require.NoError(t, err)
	defer cleanup()
	require.NotNil(t, si)

	// Resolve the fixture's actual node IDs so the synthetic membership
	// maps every one of them to our known region.
	store, ok := g.(*graph.MemoryStore)
	require.True(t, ok, "in-process build returns a MemoryStore")
	file := filepath.Join(dir, "main.go")
	nodeIDs := store.NodesForPath(file)
	require.NotEmpty(t, nodeIDs, "fixture must produce at least one node after Ingest")

	const knownRegion = 42
	membership := make(map[string]int, len(nodeIDs))
	for _, id := range nodeIDs {
		membership[id] = knownRegion
	}
	cr := &graph.CommunityResult{
		Communities: []graph.Community{{ID: knownRegion, Members: nodeIDs}},
		Membership:  membership,
	}
	mock := &countingSheafBackend{response: []int{knownRegion}}

	// Install state via the atomic SetState — same path the real
	// get_communities handler uses post-PushTopology (PR #383 #6).
	prior := si.SetState(cr, mock)
	require.Nil(t, prior, "fresh invalidator has no prior backend")

	// Edit the file. The watcher's debounce is 100ms by default.
	require.NoError(t, os.WriteFile(file, []byte("package main\nfunc Baz() { _ = 1 }\n"), 0o600))

	// Wait up to 2s for the cascade call to be recorded. Without
	// the watcher→invalidator wiring this loop times out with
	// CallCount() == 0 — that's the regression signal.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if mock.CallCount() > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	calls := mock.Calls()
	require.NotEmpty(t, calls, "watcher must route the edit through SheafBackend.Invalidate within 2s — empty here means the watcher→invalidator wiring regressed")
	// Every recorded call MUST be for the known region. Any other
	// region ID means membership-snapshot logic regressed (e.g. the
	// onChange path is using new IDs without including the pre-edit
	// snapshot — but in this test old IDs == new IDs because we
	// preserved the function name).
	for _, region := range calls {
		assert.Equal(t, knownRegion, region, "all cascades must hit the known region; got %v", region)
	}
}

// TestBuildServeGraph_IngestErrorReturnsNilInvalidator pins that error
// paths return a CONSISTENT shape — no graph, no invalidator, a noop
// cleanup, and an error. Without this, a caller that ignored the
// invalidator value but checked err could end up with a dangling
// non-nil invalidator pointing at a partially-constructed store.
//
// We trigger the ingest error by pointing at a nonexistent path; the
// engine surfaces "no such file" while still letting the cleanup chain
// complete (resolver.Close()).
func TestBuildServeGraph_IngestErrorReturnsNilInvalidator(t *testing.T) {
	t.Setenv("MACHE_NO_LEYLINE", "1")
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	g, si, cleanup, err := buildServeGraph(missing, &api.Topology{Version: api.SchemaVersion})
	require.Error(t, err, "nonexistent source must error")
	assert.Nil(t, g, "no graph returned on error")
	assert.Nil(t, si, "no invalidator returned on error")
	require.NotNil(t, cleanup, "cleanup must always be non-nil so deferred callers don't nil-deref")
	cleanup() // noop, must not panic
}

// TestBuildServeGraph_OnDeleteAlsoFiresInvalidator pins the symmetric
// path: when a file is removed, the watcher's onDelete callback must
// also route through the invalidator. Without this, deleted files leak
// stale entries in downstream caches that the cascade was meant to
// flush.
//
// The test creates a file, lets the initial ingest see it, deletes it,
// then asserts the store's NodesForPath returns empty within the
// debounce window. Pins the wiring at a black-box level — a future
// refactor that reverts onDelete to "just DeleteFileNodes, no cascade"
// is caught by TDD #4's cascade-coupling test below; this test pins
// the more basic invariant that delete-events fire at all.
func TestBuildServeGraph_OnDeleteAlsoFiresInvalidator(t *testing.T) {
	t.Setenv("MACHE_NO_LEYLINE", "1")
	dir := writeSourceDir(t, "Doomed")

	g, si, cleanup, err := buildServeGraph(dir, &api.Topology{Version: api.SchemaVersion})
	require.NoError(t, err)
	defer cleanup()
	require.NotNil(t, si)

	target := filepath.Join(dir, "main.go")
	require.NoError(t, os.Remove(target))

	store, ok := g.(*graph.MemoryStore)
	require.True(t, ok, "in-process build returns a MemoryStore")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(store.NodesForPath(target)) == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	assert.Empty(t, store.NodesForPath(target), "onDelete must purge the file's nodes within debounce + 2s")
}

// ---------------------------------------------------------------------------
// TDD #4 — get_communities populates the invalidator
// ---------------------------------------------------------------------------
//
// Without this wiring, the watcher's InvalidateWithCascade calls
// degrade to single-node Graph.Invalidate forever — even after the
// MCP layer has community data that COULD enable the cascade. The
// handler is the natural place to install it: it's the only path
// that runs community detection AND dials the daemon, which are
// the two prerequisites SheafInvalidator needs.

// testGraphWithSI is a minimal wrapper that satisfies graph.Graph,
// graph.RefsMapper (via the embedded MemoryStore), AND the
// sheafInvalidatorProvider interface — lets unit tests stand in for
// a lazyGraph without building the full lazy-init / session-routing
// machinery. Embedding the concrete *graph.MemoryStore (not the
// interface) so RefsMap/DefsMap and friends are promoted.
type testGraphWithSI struct {
	*graph.MemoryStore
	si *graph.SheafInvalidator
}

func (t *testGraphWithSI) SheafInvalidator() *graph.SheafInvalidator { return t.si }

// TestGetCommunities_PopulatesInvalidator_WithDaemon pins the consumer-
// side wire the audit identified as load-bearing: when the handler can
// reach the daemon AND PushTopology succeeds, the invalidator MUST be
// populated with BOTH the new CommunityResult and the new SheafClient
// — atomically (PR #383 Copilot #6). Pre-fix the handler did
// SetCommunityResult sync + SetSheaf async, opening a window where
// the watcher could pair new membership with stale sheaf state.
//
// We use a mock UDS daemon (sheaf_set_topology returns ok) to drive
// PushTopology to success; LEYLINE_SOCKET steers the discovery there.
func TestGetCommunities_PopulatesInvalidator_WithDaemon(t *testing.T) {
	leyline.StopManaged()
	sockPath := startMockSheafServer(t, func(req map[string]any) map[string]any {
		// PushTopology sends sheaf_set_topology; respond with the
		// success shape the daemon would return.
		return map[string]any{"ok": true, "delta_zero_mode": true, "regions": 2.0, "restrictions": 1.0}
	})
	t.Setenv("LEYLINE_SOCKET", sockPath)
	// Steer PATH/HOME away so DiscoverSocket finds our mock — not
	// some leftover real daemon — and DiscoverOrStart doesn't try
	// to auto-spawn.
	t.Setenv("PATH", "/nonexistent-path-for-test")
	t.Setenv("HOME", t.TempDir())

	// Build a store with enough cross-references to produce a real
	// CommunityResult (Louvain needs at least one community above
	// min_size).
	store := graph.NewMemoryStore()
	require.NoError(t, store.AddRef("alpha", "a1"))
	require.NoError(t, store.AddRef("alpha", "a2"))
	require.NoError(t, store.AddRef("alpha", "a3"))
	require.NoError(t, store.AddRef("beta", "b1"))
	require.NoError(t, store.AddRef("beta", "b2"))
	require.NoError(t, store.AddRef("beta", "b3"))
	require.NoError(t, store.AddRef("bridge", "a1"))
	require.NoError(t, store.AddRef("bridge", "b1"))

	// Wrap with an invalidator that the handler MUST populate.
	si := graph.NewSheafInvalidator(store, nil, nil)
	require.False(t, si.HasResult(), "precondition: invalidator starts empty")

	g := &testGraphWithSI{MemoryStore: store, si: si}

	pushDone := make(chan struct{})
	handler := makeGetCommunitiesHandlerWithDone(g, pushDone)
	result, err := handler(context.Background(), makeRequest(nil))
	require.NoError(t, err)
	require.False(t, result.IsError, "handler returned error result: %s", resultText(t, result))

	select {
	case <-pushDone:
	case <-time.After(2 * time.Second):
		t.Fatal("get_communities goroutine did not finish within 2s")
	}

	// THE CONTRACT: when PushTopology succeeds, the invalidator is
	// atomically populated (SetState swaps result + sheaf together).
	assert.True(t, si.HasResult(),
		"handler must atomically install CommunityResult + SheafClient after PushTopology succeeds — see PR #383 Copilot #6")
}

// TestGetCommunities_NoDaemon_LeavesInvalidatorEmpty pins the
// correctness side of PR #383 Copilot #6: when the daemon is
// unreachable, the handler MUST NOT install CommunityResult on the
// invalidator. Doing so would pair new membership with no sheaf
// (or, worse, stale sheaf state from a prior call) and produce
// cascades against a daemon that doesn't have the new topology.
// Graceful degradation: leave the invalidator in its prior state
// so watcher fires use the prior pair atomically.
func TestGetCommunities_NoDaemon_LeavesInvalidatorEmpty(t *testing.T) {
	t.Setenv("PATH", "/nonexistent-path-for-test")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MACHE_NO_LEYLINE", "1")
	t.Setenv("LEYLINE_SOCKET", "/tmp/nonexistent-leyline-test.sock")
	leyline.StopManaged()

	store := graph.NewMemoryStore()
	require.NoError(t, store.AddRef("a", "n1"))
	require.NoError(t, store.AddRef("a", "n2"))
	require.NoError(t, store.AddRef("b", "n2"))
	require.NoError(t, store.AddRef("b", "n3"))

	si := graph.NewSheafInvalidator(store, nil, nil)
	g := &testGraphWithSI{MemoryStore: store, si: si}

	pushDone := make(chan struct{})
	handler := makeGetCommunitiesHandlerWithDone(g, pushDone)
	result, err := handler(context.Background(), makeRequest(nil))
	require.NoError(t, err)
	require.False(t, result.IsError)

	select {
	case <-pushDone:
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine did not finish within 2s")
	}

	assert.False(t, si.HasResult(),
		"with no daemon, CommunityResult must NOT be installed — would pair new membership with stale/missing sheaf and cause wrong cascades (PR #383 Copilot #6)")
}

// TestGetCommunities_NoInvalidatorWhenGraphDoesntProvide guards the
// nil-receiver / missing-provider case. A graph that doesn't expose a
// SheafInvalidator (e.g. control-mode lazyGraph, composite mounts)
// must let the handler succeed with no state mutation — the cascade
// just isn't available in those modes and that's documented behavior.
func TestGetCommunities_NoInvalidatorWhenGraphDoesntProvide(t *testing.T) {
	t.Setenv("PATH", "/nonexistent-path-for-test")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MACHE_NO_LEYLINE", "1")
	t.Setenv("LEYLINE_SOCKET", "/tmp/nonexistent-leyline-test.sock")
	leyline.StopManaged()

	store := graph.NewMemoryStore()
	require.NoError(t, store.AddRef("x", "n1"))
	require.NoError(t, store.AddRef("x", "n2"))

	// store is a plain MemoryStore — does NOT implement
	// sheafInvalidatorProvider. Handler must complete normally.
	pushDone := make(chan struct{})
	handler := makeGetCommunitiesHandlerWithDone(store, pushDone)
	result, err := handler(context.Background(), makeRequest(nil))
	require.NoError(t, err)
	require.False(t, result.IsError, "handler must succeed even when graph has no invalidator")

	select {
	case <-pushDone:
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine did not finish within 2s")
	}
}

// ---------------------------------------------------------------------------
// TDD #5 — get_sheaf_status MCP tool [mache-4a0c05]
// ---------------------------------------------------------------------------
//
// Surfaces the daemon's monotonic generation counter (+ defect, valid,
// total) to agents so they can decide whether a cached result is still
// fresh. Pre-cmd-c14c43 the agent has no signal that the cascade fired
// at all — this tool is the missing visibility.

// startMockSheafServer is a self-contained UDS server that handles a
// single op pattern: receive line-delimited JSON, return the canned
// response from the handler func. Mirrors the (unexported)
// internal/leyline test helper; kept local to cmd/ so the package
// boundary stays clean.
func startMockSheafServer(t *testing.T, handler func(map[string]any) map[string]any) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "mache-sheaf-tool-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "t.sock")

	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				rd := bufio.NewReader(c)
				for {
					line, err := rd.ReadString('\n')
					if err != nil {
						return
					}
					var req map[string]any
					if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &req); err != nil {
						continue
					}
					resp := handler(req)
					data, _ := json.Marshal(resp)
					_, _ = c.Write(append(data, '\n'))
				}
			}(conn)
		}
	}()

	return sockPath
}

// TestGetSheafStatus_ReturnsDaemonState pins the happy path: when the
// daemon is reachable and reports sheaf state, the tool returns a JSON
// object with the four fields agents need (generation, valid, total,
// defect) plus an `available: true` marker.
//
// Uses the quoted-string generation format (the live wire shape, see
// PR #382's parseUint64 + cmd/sheaf-wire-check) to also pin that the
// MCP tool routes through SheafClient.Status (which knows that codec)
// rather than parsing the response directly and silently dropping the
// generation value.
func TestGetSheafStatus_ReturnsDaemonState(t *testing.T) {
	leyline.StopManaged()
	sockPath := startMockSheafServer(t, func(req map[string]any) map[string]any {
		assert.Equal(t, "sheaf_status", req["op"])
		return map[string]any{
			"generation": "42", // quoted — capnp-json Int64 shape
			"valid":      7.0,
			"total":      10.0,
			"defect":     0.25,
		}
	})
	t.Setenv("LEYLINE_SOCKET", sockPath)

	handler := makeGetSheafStatusHandler(nil)
	result, err := handler(context.Background(), makeRequest(nil))
	require.NoError(t, err)
	require.False(t, result.IsError, "handler must succeed when daemon responds: %s", resultText(t, result))

	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &out))
	assert.Equal(t, true, out["available"])
	assert.EqualValues(t, 42, out["generation"], "must parse quoted-string Int64 from the live wire format")
	assert.EqualValues(t, 7, out["valid"])
	assert.EqualValues(t, 10, out["total"])
	assert.InDelta(t, 0.25, out["defect"].(float64), 0.001)
}

// TestGetSheafStatus_NoDaemonReturnsUnavailable pins the documented
// graceful-degradation contract: when no daemon socket exists, the
// tool returns a structured "unavailable" response, NOT an MCP error.
// Agents calling this tool periodically (e.g. as part of a freshness
// check) should never see a transport-level failure just because the
// daemon happens to be down.
func TestGetSheafStatus_NoDaemonReturnsUnavailable(t *testing.T) {
	leyline.StopManaged()
	// Steer LEYLINE_SOCKET + HOME away so neither the env var nor the
	// well-known fallback resolves.
	t.Setenv("LEYLINE_SOCKET", "/tmp/nonexistent-sheaf-status-sock")
	t.Setenv("HOME", t.TempDir())

	handler := makeGetSheafStatusHandler(nil)
	result, err := handler(context.Background(), makeRequest(nil))
	require.NoError(t, err)
	require.False(t, result.IsError, "no-daemon path must not surface as an MCP error")

	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &out))
	assert.Equal(t, false, out["available"])
	assert.NotEmpty(t, out["reason"], "must explain why state is unavailable")
}

// TestGetSheafStatus_IncludesSubscriberState pins the c14c43 contract:
// when the registry has a running subscriber, get_sheaf_status surfaces
// the subscriber's state under a "subscriber" key. Without this, agents
// can read a daemon generation but have no way to tell whether mache
// has actually OBSERVED that generation via a pushed event (which is
// what tells them mache's MCP caches have been flushed).
func TestGetSheafStatus_IncludesSubscriberState(t *testing.T) {
	leyline.StopManaged()
	sockPath := startMockSheafServer(t, func(req map[string]any) map[string]any {
		return map[string]any{
			"generation": "12",
			"valid":      5.0,
			"total":      8.0,
			"defect":     0.1,
		}
	})
	t.Setenv("LEYLINE_SOCKET", sockPath)

	now := time.Now()
	subAccessor := func() (leyline.SubscriberStatus, bool) {
		return leyline.SubscriberStatus{
			State:          leyline.StateConnected,
			LastGeneration: 11,
			LastEvent:      now,
		}, true
	}

	handler := makeGetSheafStatusHandler(subAccessor)
	result, err := handler(context.Background(), makeRequest(nil))
	require.NoError(t, err)
	require.False(t, result.IsError)

	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &out))
	require.Contains(t, out, "subscriber", "must include subscriber sub-object when accessor reports ok")

	sub := out["subscriber"].(map[string]any)
	assert.Equal(t, "connected", sub["state"], "state lowercased for agent consumption")
	assert.EqualValues(t, 11, sub["last_generation"])
	assert.EqualValues(t, now.Unix(), sub["last_event_unix"])
	_, hasReason := sub["reason"]
	assert.False(t, hasReason, "reason omitted when state is connected")
}

// TestGetSheafStatus_SubscriberDisconnectedSurfaced pins the inverse:
// daemon may still respond to direct queries (per-call connection
// check), but agents need to see the subscriber's "not receiving
// events" state to know that previously-cached find_callers results
// may be stale even though sheaf_status itself succeeded.
func TestGetSheafStatus_SubscriberDisconnectedSurfaced(t *testing.T) {
	leyline.StopManaged()
	sockPath := startMockSheafServer(t, func(req map[string]any) map[string]any {
		return map[string]any{
			"generation": "20", "valid": 2.0, "total": 5.0, "defect": 0.0,
		}
	})
	t.Setenv("LEYLINE_SOCKET", sockPath)

	subAccessor := func() (leyline.SubscriberStatus, bool) {
		return leyline.SubscriberStatus{
			State:  leyline.StateDisconnected,
			Reason: "dial /tmp/x.sock: connection refused",
		}, true
	}

	handler := makeGetSheafStatusHandler(subAccessor)
	result, err := handler(context.Background(), makeRequest(nil))
	require.NoError(t, err)
	require.False(t, result.IsError)

	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &out))
	assert.Equal(t, true, out["available"], "daemon-direct check still passes")

	sub := out["subscriber"].(map[string]any)
	assert.Equal(t, "disconnected", sub["state"])
	assert.Contains(t, sub["reason"], "connection refused")
}

// TestGetSheafStatus_NilSubscriberAccessor pins the legacy compat
// path: nil accessor → omit the subscriber sub-object entirely,
// don't synthesize a fake state.
func TestGetSheafStatus_NilSubscriberAccessor(t *testing.T) {
	leyline.StopManaged()
	sockPath := startMockSheafServer(t, func(req map[string]any) map[string]any {
		return map[string]any{"generation": "1"}
	})
	t.Setenv("LEYLINE_SOCKET", sockPath)

	handler := makeGetSheafStatusHandler(nil)
	result, err := handler(context.Background(), makeRequest(nil))
	require.NoError(t, err)
	require.False(t, result.IsError)

	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &out))
	_, hasSubscriber := out["subscriber"]
	assert.False(t, hasSubscriber, "nil accessor → omit subscriber field (don't fake a state)")
}

// TestGetSheafStatus_RegisteredInToolSet pins that the tool is wired
// into the MCP server's tool list — without this assertion a future
// refactor could quietly drop the registration and the moat's
// visibility layer would silently disappear.
func TestGetSheafStatus_RegisteredInToolSet(t *testing.T) {
	store := graph.NewMemoryStore()
	registry := newGraphRegistry(".", nil)
	registry.graphs.Store(".", &lazyGraph{inner: store})

	srv := mcpServerForToolListing()
	registerMCPTools(srv, registry)

	names := listRegisteredTools(t, srv)
	assert.Contains(t, names, "get_sheaf_status",
		"get_sheaf_status must be in the MCP tool surface — see mache-4a0c05")
}

// mcpServerForToolListing constructs a bare MCP server fit for tool
// enumeration. Extracted so the get_sheaf_status registration test
// doesn't drag the full multi-tenant TestRegisterMCPTools fixture.
func mcpServerForToolListing() *server.MCPServer {
	return server.NewMCPServer("test", "1.0.0", server.WithToolCapabilities(false))
}

// listRegisteredTools queries the server's tools/list endpoint and
// returns the set of registered names. Mirrors the pattern in
// TestRegisterMCPTools (cmd/serve_test.go) — kept local so the new
// tool's regression guard is self-contained.
func listRegisteredTools(t *testing.T, s *server.MCPServer) map[string]bool {
	t.Helper()
	reqJSON := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	resp := s.HandleMessage(context.Background(), reqJSON)
	respJSON, err := json.Marshal(resp)
	require.NoError(t, err)

	var parsed struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(respJSON, &parsed))

	names := map[string]bool{}
	for _, tool := range parsed.Result.Tools {
		names[tool.Name] = true
	}
	return names
}
