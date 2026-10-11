package leyline

import (
	"errors"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The managed daemon's startup used to hold managed.mu for its whole readiness
// wait, up to MACHE_LEYLINE_START_TIMEOUT. StopManaged needs the same lock, so
// a SIGTERM during a cold start (v0.19.1's took ~12s on a 150-file corpus)
// could not stop anything until startup finished. The MCP client gave up and
// SIGKILLed mache, and the daemon it had been starting ran on as an orphan:
// 81 of them accumulated from release-parity runs alone (mache-25f49e).

// TestHelperFakeLeylineDaemon is not a test. It is the fake daemon the tests
// below spawn, by re-executing the test binary (the os/exec pattern), so
// there is no dependency on a real leyline or on a scripting language.
//
// MACHE_FAKE_LEYLINE_DAEMON=never never binds a socket: a startup that does
// not finish. =on-ready binds the managed socket path when it receives
// SIGUSR1, so a test decides exactly when startup completes instead of racing
// a timer. Both exit on SIGTERM. Blocking on signals, not a sleep, also keeps
// the runtime from reporting an idle helper as deadlocked.
func TestHelperFakeLeylineDaemon(t *testing.T) {
	mode := os.Getenv("MACHE_FAKE_LEYLINE_DAEMON")
	if mode == "" {
		t.Skip("helper process for the managed-daemon startup tests")
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGUSR1)
	// Announce that the handlers are installed. Until then SIGUSR1's default
	// action would kill the process, so the test waits for this file.
	dir := filepath.Join(os.Getenv("HOME"), ".mache")
	if err := os.WriteFile(filepath.Join(dir, "fake-listening"), nil, 0o644); err != nil {
		os.Exit(3)
	}
	for s := range sig {
		if s == syscall.SIGTERM {
			os.Exit(0)
		}
		if mode == "on-ready" {
			break
		}
	}
	ln, err := net.Listen("unix", filepath.Join(dir, "default.sock"))
	if err != nil {
		os.Exit(3)
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			os.Exit(0)
		}
		_ = c.Close()
	}
}

// fakeDaemonEnv points DiscoverOrStart's spawn at the fake daemon, with a
// HOME short enough for a unix socket path and a start timeout long enough
// that a stop blocked behind startup cannot be confused with a fast one.
func fakeDaemonEnv(t *testing.T, mode string) string {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "ll-start-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".mache"), 0o755))

	self, err := os.Executable()
	require.NoError(t, err)
	bin := filepath.Join(home, "leyline")
	script := "#!/bin/sh\ncase \"$1\" in\n" +
		"  --version) echo 'leyline " + leylineBinaryVersion + " (open)' ;;\n" +
		"  *) exec '" + self + "' -test.run='^TestHelperFakeLeylineDaemon$' ;;\n" +
		"esac\n"
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o755))

	t.Setenv("HOME", home)
	t.Setenv("LEYLINE_SOCKET", "")
	t.Setenv(BinaryOverrideEnv, bin)
	t.Setenv("MACHE_LEYLINE_START_TIMEOUT", "60s")
	t.Setenv("MACHE_FAKE_LEYLINE_DAEMON", mode)
	t.Cleanup(StopManaged)
	return home
}

// managedPID waits for DiscoverOrStart to record the process it spawned. It
// only TRIES the lock: a startup that holds managed.mu for its whole readiness
// wait is the bug, and must fail here rather than hang the helper.
func managedPID(t *testing.T) int {
	t.Helper()
	var pid int
	require.Eventually(t, func() bool {
		if !managed.mu.TryLock() {
			return false
		}
		defer managed.mu.Unlock()
		if managed.proc == nil {
			return false
		}
		pid = managed.proc.Pid
		return true
	}, 10*time.Second, 10*time.Millisecond,
		"never saw the spawned daemon with managed.mu free: startup holds the lock through its whole readiness wait")
	// Whatever the outcome, the fake must not outlive the test.
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	return pid
}

func TestStopManaged_DoesNotWaitForAnInFlightStartup(t *testing.T) {
	fakeDaemonEnv(t, "never")
	started := make(chan error, 1)
	go func() {
		_, err := DiscoverOrStart()
		started <- err
	}()
	pid := managedPID(t)

	stopped := make(chan struct{})
	go func() {
		StopManaged()
		close(stopped)
	}()
	require.Eventually(t, func() bool { return isClosed(stopped) }, 10*time.Second, 10*time.Millisecond,
		"StopManaged blocked behind an in-flight startup (start timeout is 60s); "+
			"a SIGTERM during a cold start cannot stop the daemon")

	var startErr error
	require.Eventually(t, func() bool {
		select {
		case startErr = <-started:
			return true
		default:
			return false
		}
	}, 10*time.Second, 10*time.Millisecond, "the startup did not notice its daemon was stopped")
	require.Error(t, startErr, "a startup whose daemon was stopped must not report success")
	assert.True(t, errors.Is(syscall.Kill(pid, 0), syscall.ESRCH),
		"the daemon (pid %d) must be gone after StopManaged", pid)
}

// A second caller arriving during a startup must wait for it. Probing the
// not-yet-bound socket and treating the daemon as stale would kill the very
// daemon that is starting.
func TestDiscoverOrStart_ConcurrentCallerWaitsForTheStartup(t *testing.T) {
	home := fakeDaemonEnv(t, "on-ready")
	first := make(chan string, 1)
	go func() {
		sock, err := DiscoverOrStart()
		assert.NoError(t, err)
		first <- sock
	}()
	pid := managedPID(t)

	second := make(chan string, 1)
	go func() {
		sock, err := DiscoverOrStart()
		assert.NoError(t, err)
		second <- sock
	}()
	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(home, ".mache", "fake-listening"))
		return err == nil
	}, 10*time.Second, 10*time.Millisecond, "the fake daemon never installed its signal handlers")
	require.NoError(t, syscall.Kill(pid, syscall.SIGUSR1), "tell the fake daemon to finish starting")

	want := filepath.Join(home, ".mache", "default.sock")
	for _, ch := range []chan string{first, second} {
		var got string
		require.Eventually(t, func() bool {
			select {
			case got = <-ch:
				return true
			default:
				return false
			}
		}, 20*time.Second, 10*time.Millisecond, "a caller never returned once the daemon was ready")
		assert.Equal(t, want, got)
	}
	managed.mu.Lock()
	defer managed.mu.Unlock()
	require.NotNil(t, managed.proc, "the starting daemon was discarded")
	assert.Equal(t, pid, managed.proc.Pid, "the starting daemon was replaced rather than awaited")
}

// isClosed reports whether ch has been closed, without blocking.
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// After shutdown, nothing may be spawned. The daemon used to be started by a
// background trigger once the graph loaded; a session that closed first ran
// StopManaged with nothing to stop, then the trigger spawned a daemon, and the
// process exited underneath it. The daemon leads its own process group, so it
// survived as an orphan (found by the parity gate's leak check).
func TestShutdownManaged_RefusesALaterSpawn(t *testing.T) {
	home := fakeDaemonEnv(t, "never")
	// A spawn that slipped through would time out and be discarded, which
	// would look like a refusal; keep that failure fast, and judge by whether
	// the fake ever ran (it writes fake-listening as soon as it starts).
	t.Setenv("MACHE_LEYLINE_START_TIMEOUT", "3s")
	t.Cleanup(func() {
		managed.mu.Lock()
		managed.shuttingDown = false
		managed.mu.Unlock()
	})

	ShutdownManaged()
	_, err := DiscoverOrStart()

	require.Error(t, err, "a spawn after shutdown would orphan its daemon")
	assert.Contains(t, err.Error(), "shutting down")
	assert.NoFileExists(t, filepath.Join(home, ".mache", "fake-listening"),
		"a daemon was started after shutdown")
}
