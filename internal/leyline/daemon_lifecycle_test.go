package leyline

import (
	"os"
	"strings"
	"testing"
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
