package leyline

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// assertNoSurvivorsFor fails if any process still holds tdir in its argv after
// the test's cleanup has run.
//
// WHAT THIS CATCHES, and why signalling the daemon alone did not.
//
// `leyline daemon` spawns `mache serve --control <ctrl>` as a CHILD
// (procgroup_unix.go documents this; it is why setProcessGroup exists at all).
// A cleanup that signals only the direct process reaps the daemon and leaves
// the grandchild running, reparented to init, pointing at a tmpdir the same
// cleanup just deleted.
//
// Measured 2026-07-30: 84 such orphans had accumulated on one machine (~1.1 GB
// resident, oldest ~6h), and a single `go test -run Sheaf` reliably added two
// more — one per sheaf e2e test. They are not merely waste: they share
// ~/.mache/default.arena, which is a documented source of smell-count jitter,
// so a leaked daemon silently perturbs later gate results.
//
// Asserting on argv rather than on a PID is deliberate: the defect is
// specifically that the surviving process is one we never held a handle to.
func assertNoSurvivorsFor(t *testing.T, tdir string) {
	t.Helper()
	// The group signal is asynchronous, so a survivor check immediately after
	// cleanup would be flaky. assert.Eventually polls the CONDITION (no process
	// holds tdir) rather than sleeping a fixed wall-clock guess — the pattern
	// the sleep_in_test rule asks for, and it also reports promptly when the
	// tree is correctly reaped instead of always paying the full budget.
	// The closure must not write a variable this function later reads:
	// assert.Eventually runs the condition in a GOROUTINE, so on the timeout
	// return path a spawned check can still be inside survivorsFor while we
	// read. Re-query in the failure branch instead — it costs one extra `ps`
	// only when already failing, and a DATA RACE abort here would bury the
	// argv message that is the entire point of the assertion.
	ok := assert.Eventually(t, func() bool {
		return survivorsFor(t, tdir) == ""
	}, 3*time.Second, 100*time.Millisecond)
	if !ok {
		t.Errorf("processes survived cleanup holding %s — the daemon's child was orphaned, "+
			"not reaped with it (use setProcessGroup at spawn + signalProcessGroup at cleanup):\n%s",
			tdir, survivorsFor(t, tdir))
	}
}

// survivorsFor returns the argv lines of live processes mentioning tdir,
// excluding this test binary itself.
func survivorsFor(t *testing.T, tdir string) string {
	t.Helper()
	out, err := exec.Command("ps", "-eo", "pid,args").Output()
	if err != nil {
		// Returning "" here would report "no survivors" — a SILENT PASS for an
		// assertion that never ran. By this repo's own standard a check that
		// cannot fire is worse than none, and on every platform mache supports
		// a missing or erroring `ps` means something is badly wrong rather than
		// that the orphan question is moot.
		t.Fatalf("cannot check for orphaned daemons: ps failed: %v", err)
	}
	var hits []string
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, tdir) {
			continue
		}
		// The `ps` we just ran, and the go test process, can mention the
		// path without being leaked daemons.
		if strings.Contains(line, "ps -eo") || strings.Contains(line, "go test") {
			continue
		}
		hits = append(hits, strings.TrimSpace(line))
	}
	return strings.Join(hits, "\n")
}
