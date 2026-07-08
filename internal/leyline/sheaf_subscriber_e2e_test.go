package leyline

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestE2E_SheafSubscriber_AgainstLiveDaemon is the cross-runtime gate
// for daemon-pushed sheaf.invalidate events (bead mache-c14c43,
// regression-paired with ley-line-open-5caa59).
//
// The discipline this test enforces: unit tests with the
// startSubscribeMockServer harness prove the subscriber's LOCAL logic
// is correct against a synthetic UDS server. They do NOT prove the
// real daemon emits the event when sheaf_invalidate is processed.
// The class of bug that escaped to live runtime was exactly that:
// cascade math correct, response correct, but the daemon's event bus
// never publishes the sheaf.invalidate topic. Mock tests would never
// catch it because they're the ones pretending to be the daemon.
//
// What this test asserts (black-box, full wire):
//
//  1. A real leyline daemon subprocess is up.
//  2. SheafSubscriber dials, subscribes, transitions to StateConnected.
//  3. Pushing sheaf_invalidate through a SEPARATE SocketClient causes
//     the subscriber's handler to fire within budget.
//  4. The event payload round-trips: Invalidated region IDs,
//     monotonic Generation, Count.
//
// Two separate SocketClients (one for ops, one for the subscriber)
// match production wiring — the Subscribe primitive owns the read
// side of its conn after activation, so sharing with SheafClient
// would race per SocketClient.Subscribe's docstring.
//
// Skipped when `leyline` is not on PATH (same gate as
// TestE2E_SheafCascade_AgainstLiveDaemon + the cascade benches).
//
// REGRESSION GUARD for ley-line-open-5caa59: pre-LLO-v0.4.3 daemons
// processed sheaf_invalidate correctly (cascade in the op response)
// but never emitted the sheaf.invalidate topic to UDS subscribers
// — handle_connection didn't drain ConnectionState.event_rx. v0.4.3
// shipped the per-connection writer + event-relay tasks. If a future
// refactor regresses this path, this test catches it before the bug
// reaches live runtime.
func TestE2E_SheafSubscriber_AgainstLiveDaemon(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate ~/.mache/ from sibling tests
	leylineBin, err := exec.LookPath("leyline")
	if err != nil {
		t.Skip("leyline binary not on PATH — skipping cross-runtime subscriber e2e")
	}

	// Mache pins to LLO v0.6+ (PR #147 unified emit under
	// `daemon.sheaf.invalidate`; the pre-v0.6 `sheaf.invalidate` topic
	// is no longer subscribed). A stale local leyline (v0.5.x) would
	// emit only on the retired topic and this test would falsely trip
	// the "daemon did not publish" assertion. Skip cleanly instead —
	// CI installs a fresh binary; local devs get a clear reason string.
	if reason := leylinePreV06Skip(leylineBin); reason != "" {
		t.Skip(reason)
	}

	sockPath, daemonCleanup := startDaemonForE2E(t, leylineBin)
	defer daemonCleanup()

	// Dedicated ops conn — used to push topology + sheaf_invalidate.
	// MUST be separate from the subscriber's conn (subscribe owns
	// the read side after activation).
	opsSock, err := DialSocket(sockPath)
	require.NoError(t, err, "dial ops socket")
	defer func() { _ = opsSock.Close() }()

	sc := NewSheafClient(opsSock)

	// Wire the subscriber FIRST, before any invalidate, so we can't
	// miss an event the daemon emits between push and subscribe.
	var (
		handled  atomic.Int32
		eventMu  sync.Mutex
		lastEvt  SheafInvalidateEvent
		evHandle = func(ev SheafInvalidateEvent) {
			eventMu.Lock()
			lastEvt = ev
			eventMu.Unlock()
			handled.Add(1)
		}
	)
	sub := NewSheafSubscriber(sockPath, evHandle)
	// Tight backoff so a stillborn daemon recycle doesn't waste the
	// test budget. Production uses 1s..30s; here we want the test
	// to finish fast on real failures.
	sub.SetBackoffForTest(50*time.Millisecond, 200*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sub.Start(ctx)
	t.Cleanup(sub.Stop)

	// Wait for the subscriber to actually reach Connected — otherwise
	// the invalidate might fire before subscribe has been activated
	// daemon-side, which would look like the bug we're testing for
	// but would actually be a race in the test setup.
	require.Eventually(t, func() bool {
		return sub.Status().State == StateConnected
	}, 5*time.Second, 25*time.Millisecond,
		"subscriber must connect before we can fairly assert event delivery")

	// Push a synthetic 2-region topology with δ⁰ inputs engaged.
	// Smaller than the cascade e2e test (3 regions there) because we
	// only need ONE invalidate to assert the event-bus path; the
	// cascade semantics themselves are covered by the existing e2e.
	regions := []region{
		{ID: 1, Hash: "aaaaaaaa", Data: bench32D(1.0)},
		{ID: 2, Hash: "bbbbbbbb", Data: bench32D(2.0)},
	}
	restrictions := []restriction{
		{A: 1, B: 2, BoundaryHash: "ab", CoChangeRate: 0.5, AgreementDim: agreementDim},
	}
	require.NoError(t, pushTopologyForE2E(sc, regions, restrictions),
		"sheaf_set_topology against live daemon")

	// Now the trigger: push sheaf_invalidate. Daemon response carrying
	// the cascade is unrelated to the event bus — we only need it to
	// succeed so the daemon ran the op at all.
	mutatedStalk := bench32D(99.0)
	_, err = sc.InvalidateWithStalk(1, "post-mutation", mutatedStalk)
	require.NoError(t, err, "sheaf_invalidate against live daemon")

	// The contract: within a generous budget (the cascade itself is
	// ~20µs on this hardware, but we allow for goroutine wakeup +
	// JSON decode on the read path), the subscriber's handler MUST
	// have fired at least once.
	//
	// PRE-LLO-FIX: this times out at handled == 0, which is the
	// signal the daemon's event bus isn't publishing sheaf.invalidate.
	// POST-LLO-FIX: handler fires inside ~10ms and the assertions
	// below pin the payload shape.
	require.Eventuallyf(t, func() bool {
		return handled.Load() > 0
	}, 3*time.Second, 25*time.Millisecond,
		"subscriber.handler never fired — daemon did not publish sheaf.invalidate to the event bus (see ley-line-open-5caa59)")

	eventMu.Lock()
	got := lastEvt
	eventMu.Unlock()

	assert.Contains(t, got.Invalidated, 1,
		"invalidated region 1 must appear in the pushed event payload")
	assert.GreaterOrEqual(t, int(got.Generation), 1,
		"event generation must reflect at least one cascade")
	assert.GreaterOrEqual(t, got.Count, 1,
		"event count must reflect at least the directly-invalidated region")

	// Subscriber's Status() should mirror what we just observed —
	// the surface get_sheaf_status reports through MCP.
	status := sub.Status()
	assert.Equal(t, StateConnected, status.State,
		"subscriber still connected after event delivery")
	assert.Equal(t, got.Generation, status.LastGeneration,
		"Status.LastGeneration must match the last event's generation")
	assert.False(t, status.LastEvent.IsZero(),
		"Status.LastEvent must be populated after first event")
}

// startDaemonForE2E is the *testing.T equivalent of startDaemonForBench.
// Spawns a leyline daemon in a /tmp subdir (SUN_LEN budget — see the
// helper in sheaf_e2e_test.go for the full reasoning), waits for its
// UDS socket to appear, and returns the socket path + a cleanup func.
//
// Returning the path (not a SocketClient) because the subscriber e2e
// needs TWO clients — ops + sub — and SocketClient.Subscribe takes
// ownership of the conn's read side, so the helper can't pre-dial.
func startDaemonForE2E(t *testing.T, leylineBin string) (string, func()) {
	t.Helper()
	tdir, err := os.MkdirTemp("/tmp", "sheaf-sub-e2e-")
	require.NoError(t, err)

	arena := filepath.Join(tdir, "arena.bin")
	ctrl := filepath.Join(tdir, "test.ctrl")
	sockPath := filepath.Join(tdir, "test.sock")

	daemon := exec.Command(leylineBin, "daemon",
		"--arena", arena,
		"--control", ctrl,
		"--timeout", "60s",
	)
	logFile, err := os.Create(filepath.Join(tdir, "daemon.log"))
	require.NoError(t, err)
	daemon.Stdout = logFile
	daemon.Stderr = logFile
	require.NoError(t, daemon.Start(), "spawn leyline daemon")

	// Poll for socket. The daemon binds asynchronously after arena
	// init — 5s is well over the typical 100ms cold-start time.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, statErr := os.Stat(sockPath); statErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	cleanup := func() {
		if daemon.Process != nil {
			_ = daemon.Process.Signal(syscall.SIGTERM)
			done := make(chan struct{})
			go func() {
				_, _ = daemon.Process.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				_ = daemon.Process.Kill()
				<-done
			}
		}
		if t.Failed() {
			// Dump daemon log on failure so the failure mode (bind
			// error, panic, unknown op) is in the test output rather
			// than discarded with the tmpdir.
			if b, readErr := os.ReadFile(filepath.Join(tdir, "daemon.log")); readErr == nil {
				t.Logf("daemon.log (on failure):\n%s", b)
			}
		}
		_ = logFile.Close()
		_ = os.RemoveAll(tdir)
	}

	// Verify the socket appeared — failing here means the daemon
	// crashed at startup, distinguishable from "subscribe never
	// gets the event" which is the actual class of bug under test.
	if _, statErr := os.Stat(sockPath); statErr != nil {
		cleanup()
		t.Fatalf("leyline daemon socket %s did not appear within 5s — daemon failed to start", sockPath)
	}

	return sockPath, cleanup
}

// pushTopologyForE2E is the *testing.T-flavored sheaf_set_topology
// pusher. Mirrors pushTopologyForBench but takes *testing.T for
// require.NoError consistency with the rest of the e2e file.
func pushTopologyForE2E(sc *SheafClient, regs []region, rests []restriction) error {
	req := map[string]any{
		"op":             "sheaf_set_topology",
		"regions":        regs,
		"restrictions":   rests,
		"node_stalk_dim": stalkDim,
	}
	resp, err := sc.sock.SendOp(req)
	if err != nil {
		return err
	}
	if e, ok := resp["error"]; ok {
		return &topologyError{msg: toStr(e)}
	}
	return nil
}

// leylinePreV06Skip returns a non-empty skip reason if the leyline
// binary at bin is a pre-v0.6 release (or its version is unreadable —
// treated as "skip cleanly" since we can't confirm the daemon speaks
// the unified `daemon.sheaf.invalidate` topic). Returns "" when the
// binary is v0.6+ and the test should run.
//
// `leyline --version` prints `leyline <semver> (open)` on one line.
// Anything else (custom builds, garbled output) is treated as
// unknown and skipped.
func leylinePreV06Skip(bin string) string {
	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		return "unable to read leyline --version output; skipping cross-runtime subscriber e2e"
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 || fields[0] != "leyline" {
		return "unrecognized leyline --version output; skipping cross-runtime subscriber e2e"
	}
	parts := strings.SplitN(fields[1], ".", 3)
	if len(parts) < 2 {
		return "unparseable leyline version; skipping cross-runtime subscriber e2e"
	}
	major, majErr := strconv.Atoi(parts[0])
	minor, minErr := strconv.Atoi(parts[1])
	if majErr != nil || minErr != nil {
		return "non-numeric leyline version; skipping cross-runtime subscriber e2e"
	}
	if major == 0 && minor < 6 {
		return "leyline " + fields[1] + " is pre-v0.6 (emits retired `sheaf.invalidate` topic); mache subscribes only to `daemon.sheaf.invalidate` (LLO PR #147)"
	}
	return ""
}
