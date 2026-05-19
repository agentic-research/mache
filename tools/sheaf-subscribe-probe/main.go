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

// coverage:ignore — diagnostic probe CLI; smoke-tested manually against
// a live leyline daemon during PR #384 development. Mirrors the pattern
// used in tools/coverage-gate/main.go for tool main() functions. Reduction
// tracked in mache-89b5dd (post-decomposition annotation-reduction campaign).
func main() { // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
	sockPath := os.Getenv("LEYLINE_SOCKET") // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
	if sockPath == "" {                     // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
		sockPath = "/tmp/mache-c14c43-e2e/test.sock" // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
	} // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.

	// Two separate SocketClients — one for ops, one for subscribe.
	// This is the same separation mache enforces in production (per
	// PR #383 SocketClient docs + c14c43 design call #1).
	opsSock, err := leyline.DialSocket(sockPath) // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
	if err != nil {                              // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
		die("dial ops sock: %v", err) // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
	} // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
	defer func() { _ = opsSock.Close() }() // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.

	subSock, err := leyline.DialSocket(sockPath) // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
	if err != nil {                              // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
		die("dial sub sock: %v", err) // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
	} // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
	defer func() { _ = subSock.Close() }() // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.

	// Subscribe FIRST so we don't miss the event.
	// Try the broadest possible pattern — if even THIS doesn't deliver
	// sheaf.invalidate, the bug is daemon-side emission, not filter.
	evCh, err := subSock.Subscribe([]string{"**"}) // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
	if err != nil {                                // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
		die("subscribe: %v", err) // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
	} // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
	fmt.Println("[probe] subscribed to sheaf.invalidate") // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.

	// Push topology.
	regions := []map[string]any{ // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
		{"id": 1, "hash": "aaaaaaaa", "data": stalk(1.0)}, // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
		{"id": 2, "hash": "bbbbbbbb", "data": stalk(2.0)}, // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
	} // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
	restrictions := []map[string]any{ // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
		{"a": 1, "b": 2, "boundary_hash": "ab", "co_change_rate": 0.5, "agreement_dim": 30}, // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
	} // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
	resp, err := opsSock.SendOp(map[string]any{ // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
		"op":             "sheaf_set_topology", // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
		"regions":        regions,              // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
		"restrictions":   restrictions,         // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
		"node_stalk_dim": 32,                   // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
	}) // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
	if err != nil { // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
		die("set_topology: %v", err) // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
	} // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
	fmt.Printf("[probe] topology pushed: %s\n", jdump(resp)) // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.

	// Trigger an invalidation.
	resp, err = opsSock.SendOp(map[string]any{ // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
		"op":      "sheaf_invalidate", // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
		"regions": []int{1},           // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
		"stalks": []map[string]any{ // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
			{"id": 1, "hash": "aaaaaaaa-mutated", "data": stalk(99.0), "agreement_dim": 30}, // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
		}, // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
	}) // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
	if err != nil { // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
		die("invalidate: %v", err) // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
	} // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
	fmt.Printf("[probe] invalidate sent: %s\n", jdump(resp)) // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.

	// Observe the event arrives on the subscription channel.
	//
	// IMPORTANT: the daemon's Subscribe op publishes ALL events,
	// regardless of which topics the client passed. (Confirmed
	// empirically: a `daemon.files.changed` from the git-watcher
	// poll comes through even when we subscribed to sheaf.invalidate
	// only.) Mache's production SheafSubscriber.dispatch silently
	// filters to its target topic — this probe needs the same logic.
	t0 := time.Now()                            // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
	deadline := time.Now().Add(3 * time.Second) // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
	for time.Now().Before(deadline) {           // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
		remaining := time.Until(deadline) // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
		select {                          // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
		case ev := <-evCh: // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
			topic, _ := ev["topic"].(string) // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
			if topic != "sheaf.invalidate" { // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
				fmt.Printf("[probe] skipping non-target event (topic=%q)\n", topic) // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
				continue                                                            // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
			} // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
			dur := time.Since(t0)                                                                 // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
			fmt.Printf("[probe] sheaf.invalidate event received in %v: %s\n", dur, jdump(ev))     // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
			fmt.Println("\nOK — daemon event bus delivers sheaf.invalidate end-to-end.")          // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
			fmt.Println("(This is the same path mache's SheafSubscriber consumes; if THIS probe") // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
			fmt.Println(" sees the event, mache's subscriber observes it too.)")                  // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
			return                                                                                // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
		case <-time.After(remaining): // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
			die("no sheaf.invalidate event received within 3s — event bus broken or filter regression") // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
		} // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
	} // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
	die("loop exit without event") // coverage:ignore — probe CLI; see main() banner. mache-89b5dd.
}

func stalk(seed float32) []float32 { // coverage:ignore — probe CLI helper; see main() banner. mache-89b5dd.
	v := make([]float32, 32) // coverage:ignore — probe CLI helper; see main() banner. mache-89b5dd.
	for i := range v {       // coverage:ignore — probe CLI helper; see main() banner. mache-89b5dd.
		v[i] = seed + float32(i)*0.01 // coverage:ignore — probe CLI helper; see main() banner. mache-89b5dd.
	} // coverage:ignore — probe CLI helper; see main() banner. mache-89b5dd.
	return v // coverage:ignore — probe CLI helper; see main() banner. mache-89b5dd.
}

func jdump(v any) string { // coverage:ignore — probe CLI helper; see main() banner. mache-89b5dd.
	b, _ := json.Marshal(v) // coverage:ignore — probe CLI helper; see main() banner. mache-89b5dd.
	return string(b)        // coverage:ignore — probe CLI helper; see main() banner. mache-89b5dd.
} // coverage:ignore — probe CLI helper; see main() banner. mache-89b5dd.

func die(format string, args ...any) { // coverage:ignore — probe CLI helper; see main() banner. mache-89b5dd.
	fmt.Fprintln(os.Stderr, "FAIL: "+fmt.Sprintf(format, args...)) // coverage:ignore — probe CLI helper; see main() banner. mache-89b5dd.
	os.Exit(1)                                                     // coverage:ignore — probe CLI helper; see main() banner. mache-89b5dd.
} // coverage:ignore — probe CLI helper; see main() banner. mache-89b5dd.
