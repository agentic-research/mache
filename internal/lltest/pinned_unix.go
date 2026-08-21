//go:build unix

package lltest

import (
	"fmt"
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

	bin := PinnedBinaryOrSkip(t)

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

// TestingT is the subset of *testing.T the resolver reports through.
// *testing.T satisfies it, so every existing caller is unchanged.
type TestingT interface {
	Helper()
	Logf(format string, args ...any)
	Skipf(format string, args ...any)
	Fatalf(format string, args ...any)
}

// Binary is which leyline a gated test resolved, and how.
type Binary struct {
	// Path to the executable.
	Path string
	// Version as the binary reports it, without a leading "v". Read from
	// --version rather than assumed, because the point of an override is that
	// it is NOT the pin.
	Version string
	// Override is true when Path came from leyline.BinaryOverrideEnv rather
	// than the pinned cache. Callers that assert pinned behaviour must consult
	// it: an override deliberately runs a different producer, so an assertion
	// phrased as "equals the pin" is meaningless and must become a report.
	Override bool
}

// decision is what ResolveBinaryOrSkip is going to do, separated from doing it.
//
// The skip-versus-fail choice is the whole safety property here, and it is
// unobservable through a real *testing.T whose Fatalf terminates the test
// asserting it. Returning it as data makes it directly testable and keeps the
// reporting down to one visible branch.
type decision struct {
	bin Binary
	// reason is non-empty when the gate cannot run as asked.
	reason string
	// fatal distinguishes "there is nothing to test" from "you asked for
	// something specific and it is broken". Only the second is a failure.
	fatal bool
}

// decideBinary resolves which leyline the gated tests should use.
//
// It honours leyline.BinaryOverrideEnv — the SAME override production's
// ResolveBinary already checks before every pinned tier. These gates used to
// call CachedPinnedBinary directly and so ignored it, which meant that setting
// it to a release candidate pointed `mache build` at the candidate while the
// conformance and parity gates silently went on testing the pin, or skipped
// (mache-cc1a70). One knob, one contract.
//
// Without the override: the pinned binary, resolved the way production does —
// version-namespaced ~/.mache/bin/leyline-<pin> first, legacy unversioned path
// only when it happens to hold the pin — and a SKIP when none is cached. It
// never downloads.
//
// Resolving the legacy path DIRECTLY (as this did until mache-7555da) made every
// pinned gate skip unconditionally once the cache became version-namespaced:
// nothing writes ~/.mache/bin/leyline any more, so on a machine whose legacy
// file was left behind by an older pin the gate could never fire.
func decideBinary() decision {
	if path, set, err := leyline.OverrideBinary(); set {
		// A named-but-broken override is FATAL, never a skip. Skipping is right
		// when there is nothing to test; here someone asked for a specific
		// binary, and silently testing nothing — or worse, the pin — would
		// report success for a validation that never ran.
		if err != nil {
			return decision{reason: err.Error(), fatal: true}
		}
		out, verr := exec.Command(path, "--version").Output()
		if verr != nil {
			return decision{
				reason: fmt.Sprintf("%s=%s is not runnable: %v", leyline.BinaryOverrideEnv, path, verr),
				fatal:  true,
			}
		}
		v := leyline.ExtractSemver(string(out))
		if v == "" {
			v = strings.TrimSpace(string(out))
		}
		return decision{bin: Binary{Path: path, Version: v, Override: true}}
	}

	bin := leyline.CachedPinnedBinary()
	if bin == "" {
		return decision{reason: fmt.Sprintf(
			"no cached leyline matching pin %s (never downloading in tests; set %s to run against another binary)",
			leyline.PinnedBinaryVersion(), leyline.BinaryOverrideEnv)}
	}
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		return decision{reason: fmt.Sprintf(
			"pinned leyline not runnable at %s (never downloading in tests): %v", bin, err)}
	}
	want := strings.TrimPrefix(leyline.PinnedBinaryVersion(), "v")
	if !strings.Contains(string(out), want) {
		return decision{reason: fmt.Sprintf(
			"leyline at %s reports %q, want pinned %s — skipping (never downloading in tests)",
			bin, strings.TrimSpace(string(out)), want)}
	}
	return decision{bin: Binary{Path: bin, Version: want}}
}

// ResolveBinaryOrSkip reports decideBinary's answer through t: fatal reasons
// fail, everything else skips, and an override announces itself so a green run
// can never be read as green against the pin.
func ResolveBinaryOrSkip(t TestingT) Binary {
	t.Helper()
	d := decideBinary()
	switch {
	case d.fatal:
		t.Fatalf("lltest: %s", d.reason)
	case d.reason != "":
		t.Skipf("lltest: %s", d.reason)
	case d.bin.Override:
		t.Logf("lltest: %s=%s reports %q — running against an OVERRIDE, not the pin (%s)",
			leyline.BinaryOverrideEnv, d.bin.Path, d.bin.Version, leyline.PinnedBinaryVersion())
	}
	return d.bin
}

// PinnedBinaryOrSkip returns just the path, for gates that only need to exec
// something and do not care which producer answered.
func PinnedBinaryOrSkip(t TestingT) string {
	t.Helper()
	return ResolveBinaryOrSkip(t).Path
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
