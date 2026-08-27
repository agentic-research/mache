//go:build unix

package leyline

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOwnerRecord_RoundTripsBesideTheSocket pins the file contract: written at
// spawn-success, readable by anyone holding the socket path, absence reported
// as absence rather than error.
func TestOwnerRecord_RoundTripsBesideTheSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "d.sock")

	_, ok := ReadOwnerRecord(sock)
	require.False(t, ok, "no record yet: absence is normal, not an error — "+
		"every pre-record daemon and every hand-started one has none")

	writeOwnerRecord(sock, 12345)

	rec, ok := ReadOwnerRecord(sock)
	require.True(t, ok)
	assert.Equal(t, 12345, rec.DaemonPID)
	assert.Equal(t, os.Getpid(), rec.SpawnerPID, "the spawner is THIS process")
	assert.Equal(t, leylineBinaryVersion, rec.Pin,
		"provenance records what the spawner REQUIRED — not what the binary reports, "+
			"which stays the daemon's own answer (a file named v0.18.2 reported 0.18.1)")
	assert.WithinDuration(t, time.Now().UTC(), rec.StartedAt, time.Minute)
}

// TestOwnerRecord_CorruptRecordReadsAsAbsent: a half-written or garbage file
// must degrade to "no provenance", never to a parse error a caller has to
// handle — the record is attribution, not a dependency.
func TestOwnerRecord_CorruptRecordReadsAsAbsent(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "d.sock")
	require.NoError(t, os.WriteFile(ownerRecordPath(sock), []byte("{not json"), 0o644))

	_, ok := ReadOwnerRecord(sock)
	assert.False(t, ok)
}

// deadPID returns a pid that is guaranteed dead: spawn a no-op child and reap
// it. Using a made-up number risks colliding with a live process; using a real
// reaped child cannot.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/usr/bin/true")
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid
	require.NoError(t, cmd.Wait())
	return pid
}

// TestOwnerRecord_OrphanedAndStaleAreDistinctStates pins the taxonomy the
// doctor reports from, with REAL pids rather than fabricated ones:
//
//	orphaned = daemon alive, spawner dead  (the 11-day-daemon state)
//	stale    = daemon dead                 (the file outlived the process)
//
// They demand different remediations — an orphan needs killing or adopting, a
// stale record needs nothing — so conflating them would put the wrong remedy
// in doctor's output.
func TestOwnerRecord_OrphanedAndStaleAreDistinctStates(t *testing.T) {
	self := os.Getpid()
	dead := deadPID(t)

	orphan := OwnerRecord{DaemonPID: self, SpawnerPID: dead}
	assert.True(t, orphan.Orphaned(), "live daemon + dead spawner = orphaned")
	assert.False(t, orphan.Stale())

	stale := OwnerRecord{DaemonPID: dead, SpawnerPID: self}
	assert.True(t, stale.Stale(), "dead daemon = stale record, whoever spawned it")
	assert.False(t, stale.Orphaned(), "a dead daemon cannot be an orphan; there is nothing to own")

	owned := OwnerRecord{DaemonPID: self, SpawnerPID: self}
	assert.False(t, owned.Orphaned())
	assert.False(t, owned.Stale())

	// BOTH dead: stale, and NOT orphaned. This fixture exists because without
	// it a mutant that drops the daemon-liveness term ("orphaned = spawner
	// dead") is EQUIVALENT on every other case here and survived mutation —
	// the two-dead record is the only input that separates the predicates.
	gone := OwnerRecord{DaemonPID: dead, SpawnerPID: deadPID(t)}
	assert.True(t, gone.Stale())
	assert.False(t, gone.Orphaned(),
		"a dead daemon with a dead spawner is a stale record, not an orphan to kill")
}

// TestOwnerRecord_ZeroAndNegativePIDsAreDead guards the footgun in kill(2):
// pid 0 signals the CALLER'S PROCESS GROUP and negative pids signal groups —
// a zero-valued record must read as dead, not probe the caller's own group and
// report the daemon alive because the test process exists.
func TestOwnerRecord_ZeroAndNegativePIDsAreDead(t *testing.T) {
	assert.False(t, processAlive(0))
	assert.False(t, processAlive(-1))
	assert.True(t, OwnerRecord{DaemonPID: 0}.Stale())
}

// TestWellKnownOwnerRecord_ReadsTheSharedSocketRecord covers doctor's entry
// point, under an isolated HOME so it never reads the developer's real record.
func TestWellKnownOwnerRecord_ReadsTheSharedSocketRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	_, ok := WellKnownOwnerRecord()
	require.False(t, ok, "fresh HOME has no record")

	require.NoError(t, os.MkdirAll(filepath.Join(home, ".mache"), 0o755))
	writeOwnerRecord(filepath.Join(home, ".mache", "default.sock"), 4242)

	rec, ok := WellKnownOwnerRecord()
	require.True(t, ok)
	assert.Equal(t, 4242, rec.DaemonPID)
}
