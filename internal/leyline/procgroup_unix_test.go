//go:build unix

package leyline

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestSignalProcessGroup_ReapsChild reproduces the daemon orphan bug
// (mache-823d91): a spawned process itself spawns a child; killing only
// the parent leaves the child orphaned. setProcessGroup + signalProcessGroup
// must reap the WHOLE tree. Here the parent shell backgrounds a child
// `sleep` (mirroring `leyline daemon` spawning `mache serve --control`),
// prints the child PID, then waits. Group-killing the parent must also
// kill the child.
func TestSignalProcessGroup_ReapsChild(t *testing.T) {
	// Parent backgrounds a long sleep (the "grandchild mache"), prints its
	// PID, then sleeps itself so the group stays alive until we kill it.
	cmd := exec.Command("sh", "-c", "sleep 60 & echo $!; sleep 60")
	setProcessGroup(cmd)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	buf := make([]byte, 64)
	n, _ := out.Read(buf)
	childPID, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil {
		t.Fatalf("parse child pid from %q: %v", string(buf[:n]), err)
	}

	// Sanity: the backgrounded child is alive before we kill the group.
	if err := syscall.Kill(childPID, 0); err != nil {
		t.Fatalf("child %d should be alive before kill: %v", childPID, err)
	}

	// Kill the whole group — must reap the parent AND the child.
	if err := signalProcessGroup(cmd.Process, syscall.SIGKILL); err != nil {
		t.Fatalf("signalProcessGroup: %v", err)
	}
	_, _ = cmd.Process.Wait()

	// The child must be gone. Poll briefly to absorb reaping latency.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(childPID, 0); err != nil {
			return // ESRCH — child reaped, success
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Clean up the leak if the fix failed, so the test doesn't orphan it.
	_ = syscall.Kill(childPID, syscall.SIGKILL)
	t.Fatalf("child %d survived group kill — orphan not reaped", childPID)
}
