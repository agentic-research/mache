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
	var last string
	ok := assert.Eventually(t, func() bool {
		last = survivorsFor(t, tdir)
		return last == ""
	}, 3*time.Second, 100*time.Millisecond)
	if !ok {
		t.Errorf("processes survived cleanup holding %s — the daemon's child was orphaned, "+
			"not reaped with it (use setProcessGroup at spawn + signalProcessGroup at cleanup):\n%s",
			tdir, last)
	}
}

// survivorsFor returns the argv lines of live processes mentioning tdir,
// excluding this test binary itself.
func survivorsFor(t *testing.T, tdir string) string {
	t.Helper()
	out, err := exec.Command("ps", "-eo", "pid,args").Output()
	if err != nil {
		t.Logf("ps unavailable (%v) — cannot check for orphans", err)
		return ""
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
