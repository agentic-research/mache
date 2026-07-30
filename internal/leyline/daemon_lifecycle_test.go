package leyline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestManagedDaemon_AllTerminationViaSeam is the consistency measure for
// the managed-daemon lifecycle: every kill site in socket.go MUST go
// through the managedDaemon seam (signalGroup / discard), which signals
// the daemon's whole process GROUP so the `mache serve --control` child it
// spawns is reaped rather than orphaned (mache-823d91).
//
// A direct os.Process.Kill() / .Signal() in socket.go would silently
// reintroduce the orphan leak this fix removed — there were FOUR such
// sites before the seam, and each had to independently remember the
// group discipline. This test makes "add a new kill site, forget the
// group-kill" a compile-green-but-test-red mistake instead of a latent leak.
//
// The legitimate process-signaling primitive lives in procgroup_unix.go
// (signalProcessGroup) and is reached only via the seam methods; this test
// deliberately scans socket.go alone.
func TestManagedDaemon_AllTerminationViaSeam(t *testing.T) {
	src, err := os.ReadFile("socket.go")
	if err != nil {
		t.Fatalf("read socket.go: %v", err)
	}
	text := string(src)
	for _, forbidden := range []string{".Kill()", ".Signal("} {
		if strings.Contains(text, forbidden) {
			t.Errorf("socket.go contains a direct process %q call — terminate the "+
				"managed daemon via managed.signalGroup/discard (the group-aware seam), "+
				"not a raw process call, or the daemon's mache child gets orphaned (mache-823d91)",
				forbidden)
		}
	}
}

// TestSpawnedDaemons_TerminateViaSeam widens the scan above to this package's
// TEST files, which is where the gate's own stated failure mode actually
// recurred.
//
// The comment on TestManagedDaemon_AllTerminationViaSeam says it exists so
// that "add a new kill site, forget the group-kill" is a compile-green-but-
// test-red mistake — but it "deliberately scans socket.go alone", so three
// test-side spawn sites (sheaf_e2e, sheaf_subscriber_e2e, sheaf_cascade_bench)
// were free to reintroduce exactly the leak it describes. They did: each
// signalled only the direct process, orphaning the `mache serve --control`
// grandchild `leyline daemon` spawns. 84 such orphans accumulated on one
// machine before anyone noticed (~1.1 GB resident).
//
// A structural scan catches this at the SITE, before any process is spawned —
// unlike assertNoSurvivorsFor, which can only observe the damage afterwards
// and only in tests that remember to call it. Both are kept: this one prevents
// the class, that one catches a leak arriving by some route this cannot see
// (e.g. a daemon that spawns a grandchild we never group).
//
// Files that legitimately hold a raw process handle can opt out by name, with
// a reason — the list is the audit trail.
func TestSpawnedDaemons_TerminateViaSeam(t *testing.T) {
	// procgroup_unix_test.go drives raw processes on purpose: it is the test
	// FOR the seam, so it must be able to construct the ungrouped case.
	exempt := map[string]string{
		"procgroup_unix_test.go": "tests the seam itself; must be able to build the ungrouped case",
		// This file NAMES the forbidden tokens as string literals — they are
		// the specification here, not calls. A scanner that cannot describe
		// what it forbids without tripping is not usable.
		"daemon_lifecycle_test.go": "contains the forbidden tokens as the scan's own literals",
	}

	entries, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatalf("glob test files: %v", err)
	}
	require.NotEmpty(t, entries, "found no *_test.go — the scan would vacuously pass")

	for _, file := range entries {
		if reason, ok := exempt[filepath.Base(file)]; ok {
			t.Logf("exempt: %s (%s)", file, reason)
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		text := string(src)
		for _, forbidden := range []string{".Process.Kill()", ".Process.Signal("} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s contains a direct process %q call — a spawned `leyline daemon` "+
					"spawns `mache serve --control` as a CHILD, so signalling only the direct "+
					"process orphans the grandchild. Use setProcessGroup at spawn and "+
					"signalProcessGroup at cleanup (procgroup_unix.go, mache-823d91).",
					file, forbidden)
			}
		}
	}
}
