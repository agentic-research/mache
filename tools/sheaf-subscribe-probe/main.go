// Smoke probe: subscribe to sheaf.invalidate events from a running
// ley-line daemon, push an invalidate, verify the subscription
// receives the pushed event. Exercises the same event-bus path
// mache's SheafSubscriber (c14c43) uses in production.
//
// Run:
//
//	LEYLINE_SOCKET=/path/to/daemon.sock go run ./tools/sheaf-subscribe-probe/
//
// Exit 0 = event received within budget; exit 1 = timeout or daemon
// error. Hard-coded ~3s budget — production cascades round-trip in
// <100µs so 3s catches both real failures and pathological GC pauses.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/agentic-research/mache/internal/leyline"
)

func main() {
	sockPath := os.Getenv("LEYLINE_SOCKET")
	if sockPath == "" {
		sockPath = "/tmp/mache-c14c43-e2e/test.sock"
	}

	// Two separate SocketClients — one for ops, one for subscribe.
	// This is the same separation mache enforces in production (per
	// PR #383 SocketClient docs + c14c43 design call #1).
	opsSock, err := leyline.DialSocket(sockPath)
	if err != nil {
		die("dial ops sock: %v", err)
	}
	defer func() { _ = opsSock.Close() }()

	subSock, err := leyline.DialSocket(sockPath)
	if err != nil {
		die("dial sub sock: %v", err)
	}
	defer func() { _ = subSock.Close() }()

	// Subscribe FIRST so we don't miss the event.
	// Try the broadest possible pattern — if even THIS doesn't deliver
	// sheaf.invalidate, the bug is daemon-side emission, not filter.
	evCh, err := subSock.Subscribe([]string{"**"})
	if err != nil {
		die("subscribe: %v", err)
	}
	fmt.Println("[probe] subscribed to sheaf.invalidate")

	// Push topology.
	regions := []map[string]any{
		{"id": 1, "hash": "aaaaaaaa", "data": stalk(1.0)},
		{"id": 2, "hash": "bbbbbbbb", "data": stalk(2.0)},
	}
	restrictions := []map[string]any{
		{"a": 1, "b": 2, "boundary_hash": "ab", "co_change_rate": 0.5, "agreement_dim": 30},
	}
	resp, err := opsSock.SendOp(map[string]any{
		"op":             "sheaf_set_topology",
		"regions":        regions,
		"restrictions":   restrictions,
		"node_stalk_dim": 32,
	})
	if err != nil {
		die("set_topology: %v", err)
	}
	fmt.Printf("[probe] topology pushed: %s\n", jdump(resp))

	// Trigger an invalidation.
	resp, err = opsSock.SendOp(map[string]any{
		"op":      "sheaf_invalidate",
		"regions": []int{1},
		"stalks": []map[string]any{
			{"id": 1, "hash": "aaaaaaaa-mutated", "data": stalk(99.0), "agreement_dim": 30},
		},
	})
	if err != nil {
		die("invalidate: %v", err)
	}
	fmt.Printf("[probe] invalidate sent: %s\n", jdump(resp))

	// Observe the event arrives on the subscription channel.
	//
	// IMPORTANT: the daemon's Subscribe op publishes ALL events,
	// regardless of which topics the client passed. (Confirmed
	// empirically: a `daemon.files.changed` from the git-watcher
	// poll comes through even when we subscribed to sheaf.invalidate
	// only.) Mache's production SheafSubscriber.dispatch silently
	// filters to its target topic — this probe needs the same logic.
	t0 := time.Now()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		select {
		case ev := <-evCh:
			topic, _ := ev["topic"].(string)
			if topic != "sheaf.invalidate" {
				fmt.Printf("[probe] skipping non-target event (topic=%q)\n", topic)
				continue
			}
			dur := time.Since(t0)
			fmt.Printf("[probe] sheaf.invalidate event received in %v: %s\n", dur, jdump(ev))
			fmt.Println("\nOK — daemon event bus delivers sheaf.invalidate end-to-end.")
			fmt.Println("(This is the same path mache's SheafSubscriber consumes; if THIS probe")
			fmt.Println(" sees the event, mache's subscriber observes it too.)")
			return
		case <-time.After(remaining):
			die("no sheaf.invalidate event received within 3s — event bus broken or filter regression")
		}
	}
	die("loop exit without event")
}

func stalk(seed float32) []float32 {
	v := make([]float32, 32)
	for i := range v {
		v[i] = seed + float32(i)*0.01
	}
	return v
}

func jdump(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func die(format string, args ...any) {
	fmt.Fprintln(os.Stderr, "FAIL: "+fmt.Sprintf(format, args...))
	os.Exit(1)
}
