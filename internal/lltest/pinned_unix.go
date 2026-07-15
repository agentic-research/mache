//go:build unix

package lltest

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agentic-research/mache/internal/leyline"
)

// UsePinnedDaemon spawns a private pinned daemon (see StartPinnedDaemon) and
// points LEYLINE_SOCKET at it for the remainder of the test, which is how
// every gated e2e consumer wires the daemon into mache's discovery path.
func UsePinnedDaemon(t *testing.T) {
	t.Helper()
	t.Setenv("LEYLINE_SOCKET", StartPinnedDaemon(t))
}

// StartPinnedDaemon spawns the SHA-pinned leyline binary from
// ~/.mache/bin/leyline as a PRIVATE daemon (own arena/control/socket in a
// short-lived temp dir — no interference with the shared ~/.mache arena) and
// returns its socket path.
//
// Gated, never downloads: the test SKIPS when the binary is absent or its
// --version does not match leyline.PinnedBinaryVersion(). It deliberately
// does NOT fall back to a leyline on PATH — an unpinned binary produces
// different wire behavior than CI's SHA-verified release (mache-608a3c).
func StartPinnedDaemon(t *testing.T) string {
	t.Helper()

	bin := pinnedBinaryOrSkip(t)

	// /tmp keeps the socket path under the ~104-byte sun_path limit; the
	// daemon binds <control-stem>.sock next to the control file.
	dir, err := os.MkdirTemp("/tmp", "llpin")
	if err != nil {
		t.Fatalf("lltest: create pinned daemon dir: %v", err)
	}

	cmd := exec.Command(bin, "daemon",
		"--arena", filepath.Join(dir, "d.arena"),
		"--arena-size-mib", "64",
		"--control", filepath.Join(dir, "d.ctrl"),
	) // headless: no --mount, no --source — validate is content-supplied
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(dir)
		t.Fatalf("lltest: start pinned leyline daemon: %v", err)
	}
	t.Cleanup(func() {
		// Group-kill so any child the daemon spawned is reaped with it.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
		_ = os.RemoveAll(dir)
	})

	return awaitSocket(t, filepath.Join(dir, "d.sock"))
}

// pinnedBinaryOrSkip resolves ~/.mache/bin/leyline and verifies it reports
// the pinned version, skipping the test otherwise.
func pinnedBinaryOrSkip(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("lltest: no home dir, cannot locate pinned leyline: %v", err)
	}
	bin := filepath.Join(home, ".mache", "bin", "leyline")
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		t.Skipf("lltest: pinned leyline not runnable at %s (never downloading in tests): %v", bin, err)
	}
	want := strings.TrimPrefix(leyline.PinnedBinaryVersion(), "v")
	if !strings.Contains(string(out), want) {
		t.Skipf("lltest: leyline at %s reports %q, want pinned %s — skipping (never downloading in tests)", bin, strings.TrimSpace(string(out)), want)
	}
	return bin
}

// awaitSocket polls until a UDS listener answers at sock or the deadline
// passes.
func awaitSocket(t *testing.T, sock string) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.Dial("unix", sock); err == nil {
			_ = conn.Close()
			return sock
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("lltest: pinned leyline daemon socket %s did not appear within 15s", sock)
	return "" // unreachable
}
