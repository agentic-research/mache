package cmd

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/agentic-research/mache/internal/leyline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRouteSheafEvent_InvalidatesMatchingRegion pins the routing
// contract that the subscriber handler delegates to: given a
// sheaf.invalidate event with region IDs and a set of registered
// SheafInvalidators (one per lazyGraph in the registry), dispatch
// the event to whichever invalidator has the matching region in its
// CommunityResult.
//
// Routing has to find the right invalidator because multiple
// lazyGraphs may share one process (multi-tenant serve mode) and
// only one of them owns the topology the daemon currently holds.
// Events for unknown regions log + skip per the c14c43 contract.
func TestRouteSheafEvent_InvalidatesMatchingRegion(t *testing.T) {
	g := &fakeGraph{}
	cr := &graph.CommunityResult{
		Communities: []graph.Community{{ID: 7, Members: []string{"a", "b"}}},
		Membership:  map[string]int{"a": 7, "b": 7},
	}
	si := graph.NewSheafInvalidator(g, nil, cr)

	ev := leyline.SheafInvalidateEvent{
		Invalidated: []int{7},
		Generation:  3,
		Count:       1,
	}

	routeSheafEvent(ev, []*graph.SheafInvalidator{si})

	// Region 7's members a + b should be invalidated.
	assert.ElementsMatch(t, []string{"a", "b"}, g.Invalidated(),
		"members of the affected region must be invalidated locally")
}

// TestRouteSheafEvent_SkipsUnmappedRegion pins the c14c43 contract
// from question #4: when the event references a region NO invalidator
// claims (e.g. event arrived before any get_communities run), the
// router logs + skips. No panic, no buffer, no fallback that touches
// unrelated graphs.
func TestRouteSheafEvent_SkipsUnmappedRegion(t *testing.T) {
	g := &fakeGraph{}
	// Invalidator with NO CommunityResult — represents the pre-
	// get_communities state.
	si := graph.NewSheafInvalidator(g, nil, nil)

	ev := leyline.SheafInvalidateEvent{
		Invalidated: []int{99},
		Generation:  1,
		Count:       1,
	}
	routeSheafEvent(ev, []*graph.SheafInvalidator{si})

	assert.Empty(t, g.Invalidated(),
		"no invalidations when no registered invalidator can map the region")
}

// TestRouteSheafEvent_MultiInvalidatorRoutesToOwner pins multi-tenant
// behavior: two invalidators registered, only one knows region 7;
// only that one's graph receives invalidations. The other graph
// must NOT be touched.
func TestRouteSheafEvent_MultiInvalidatorRoutesToOwner(t *testing.T) {
	gOwner := &fakeGraph{}
	siOwner := graph.NewSheafInvalidator(gOwner, nil, &graph.CommunityResult{
		Communities: []graph.Community{{ID: 7, Members: []string{"owner-a"}}},
		Membership:  map[string]int{"owner-a": 7},
	})

	gOther := &fakeGraph{}
	siOther := graph.NewSheafInvalidator(gOther, nil, &graph.CommunityResult{
		// Region 99 belongs to gOther, but the event is for region 7.
		Communities: []graph.Community{{ID: 99, Members: []string{"other-x"}}},
		Membership:  map[string]int{"other-x": 99},
	})

	routeSheafEvent(leyline.SheafInvalidateEvent{Invalidated: []int{7}},
		[]*graph.SheafInvalidator{siOwner, siOther})

	assert.Equal(t, []string{"owner-a"}, gOwner.Invalidated(),
		"only the invalidator whose membership claims region 7 must fire")
	assert.Empty(t, gOther.Invalidated(),
		"unrelated graphs must not be touched")
}

// TestRouteSheafEvent_MultipleRegionsInOneEvent pins that an event
// carrying multiple region IDs invalidates ALL of them. The daemon's
// cascade can report several regions in one event; dropping all but
// the first would silently break the moat.
func TestRouteSheafEvent_MultipleRegionsInOneEvent(t *testing.T) {
	g := &fakeGraph{}
	si := graph.NewSheafInvalidator(g, nil, &graph.CommunityResult{
		Communities: []graph.Community{
			{ID: 1, Members: []string{"a"}},
			{ID: 2, Members: []string{"b"}},
			{ID: 3, Members: []string{"c"}},
		},
		Membership: map[string]int{"a": 1, "b": 2, "c": 3},
	})

	routeSheafEvent(leyline.SheafInvalidateEvent{Invalidated: []int{1, 3}},
		[]*graph.SheafInvalidator{si})

	assert.ElementsMatch(t, []string{"a", "c"}, g.Invalidated(),
		"every region in the event must invalidate its members")
}

// fakeGraph is a minimal graph.Graph that records Invalidate calls
// for assertion. Mirrors the mockGraph in internal/graph but lives
// here so the cmd-package routing test isn't coupled to that file's
// (internal) test types.
type fakeGraph struct {
	mu          sync.Mutex
	invalidated []string
}

func (f *fakeGraph) Invalidate(id string) {
	f.mu.Lock()
	f.invalidated = append(f.invalidated, id)
	f.mu.Unlock()
}

func (f *fakeGraph) Invalidated() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.invalidated))
	copy(out, f.invalidated)
	return out
}

// Stubs for the Graph interface — none called by routing.
func (f *fakeGraph) GetNode(string) (*graph.Node, error)             { return nil, graph.ErrNotFound }
func (f *fakeGraph) ListChildren(string) ([]string, error)           { return nil, nil }
func (f *fakeGraph) ListChildStats(string) ([]graph.NodeStat, error) { return nil, nil }
func (f *fakeGraph) ReadContent(string, []byte, int64) (int, error)  { return 0, nil }
func (f *fakeGraph) GetCallers(string) ([]*graph.Node, error)        { return nil, nil }
func (f *fakeGraph) GetCallees(string) ([]*graph.Node, error)        { return nil, nil }
func (f *fakeGraph) Act(string, string, string) (*graph.ActionResult, error) {
	return nil, graph.ErrActNotSupported
}

// TestSheafEventRouter_SnapshotIsRaceFree pins the registration path:
// the lazyGraph registers its SheafInvalidator via the router, and
// the router's snapshot for event dispatch must be safe to read while
// other goroutines register / unregister.
//
// This is the same shape of mutex contract PR #383 caught for the
// SheafInvalidator itself — locked here at the registry level so the
// race detector flags any future regression that drops the lock.
func TestSheafEventRouter_SnapshotIsRaceFree(t *testing.T) {
	r := newSheafEventRouter()

	const writers = 4
	const iters = 50

	var done sync.WaitGroup
	for i := 0; i < writers; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			for j := 0; j < iters; j++ {
				si := graph.NewSheafInvalidator(&fakeGraph{}, nil, nil)
				r.register(si)
				time.Sleep(time.Microsecond)
				r.unregister(si)
			}
		}()
	}

	// Reader goroutine — snapshots while writers churn.
	done.Add(1)
	go func() {
		defer done.Done()
		for j := 0; j < iters*2; j++ {
			_ = r.snapshot()
		}
	}()

	require.True(t, waitTimeout(&done, 5*time.Second), "race-test goroutines must finish in 5s")
}

// waitTimeout blocks until the wait group completes or the deadline expires.
// Returns true if the group completed, false on timeout.
func waitTimeout(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// TestStartSheafSubscriber_StartsEvenWhenSocketAbsent pins the
// architectural fix from PR #384 Copilot #3. The prior
// startSheafSubscriber called DiscoverSocket up front and returned
// nil when no socket existed at startup — so any auto-spawn that
// brought a daemon up later (via get_communities, LSP enrichment,
// etc.) would never have a subscriber, and daemon-pushed
// invalidations would silently never reach mache until a full serve
// restart.
//
// The new contract: always start the subscriber with a deterministic
// path (LEYLINE_SOCKET env if set, else ~/.mache/default.sock). The
// subscriber's existing reconnect-with-backoff loop handles initial
// absence — it'll dial the path repeatedly until the daemon comes
// up, then subscribe normally.
//
// We point LEYLINE_SOCKET at a temp path that doesn't exist, call
// startSheafSubscriber, and assert the returned subscriber is
// non-nil. The Status() will report Disconnected (with a dial-error
// Reason) but the subscriber is alive and retrying.
func TestStartSheafSubscriber_StartsEvenWhenSocketAbsent(t *testing.T) {
	// Set LEYLINE_SOCKET to a guaranteed-missing path.
	missing := filepath.Join(t.TempDir(), "no-daemon.sock")
	t.Setenv("LEYLINE_SOCKET", missing)
	t.Setenv("HOME", t.TempDir()) // Also steer the fallback path away.

	router := newSheafEventRouter()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sub, stop := startSheafSubscriber(ctx, router)
	t.Cleanup(stop)

	require.NotNil(t, sub,
		"startSheafSubscriber must return a non-nil subscriber even with no socket present — backoff handles initial absence (PR #384 Copilot #3)")

	// Subscriber should be visibly trying-and-failing within the
	// initial backoff window. The state may be Connecting or
	// Disconnected depending on timing, but it MUST NOT be Connected.
	require.Eventually(t, func() bool {
		st := sub.Status().State
		return st == leyline.StateConnecting || st == leyline.StateDisconnected
	}, 1*time.Second, 25*time.Millisecond,
		"subscriber must enter the connect-retry loop (got %v)", sub.Status().State)

	// Reason should explain why it's not connected.
	st := sub.Status()
	assert.NotEqual(t, leyline.StateConnected, st.State,
		"subscriber must NOT report Connected when the socket is missing")
}
