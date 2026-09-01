package leyline

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// OwnerRecord is who started a leyline daemon, with what, and when — written
// next to the socket so "is this ours and current" is answerable without a
// round trip, and so a stale daemon can be ATTRIBUTED instead of merely
// detected.
//
// Why it exists (mache-967cff): drift detection can say "a daemon reporting
// 0.18.1 is running against a v0.19.0 pin" but not whose it is, how old it is,
// or whether its spawner died long ago. Observed twice in one week: an
// 11-day-21-hour daemon nobody could date without `ps etime`, and a leaked
// test daemon that ran 1h43m before the drift warning caught it — caught, but
// unattributable. Cleanup stayed manual `kill` by a human reading ps.
type OwnerRecord struct {
	// Pin is the leyline release the spawner required (leylineBinaryVersion
	// at spawn time), which is NOT necessarily what the binary reports —
	// asking the daemon remains the authority (a file named v0.18.2 reported
	// 0.18.1 this week). The record is provenance, not identity.
	Pin string `json:"pin"`
	// DaemonPID is the spawned daemon process.
	DaemonPID int `json:"daemon_pid"`
	// SpawnerPID is the mache process that spawned it. A dead spawner beside
	// a live daemon is the orphaned-but-alive state that used to accumulate
	// silently.
	SpawnerPID int `json:"spawner_pid"`
	// StartedAt is when the daemon reported ready.
	StartedAt time.Time `json:"started_at"`
}

// ownerRecordPath is the record's location for a given socket.
func ownerRecordPath(sockPath string) string { return sockPath + ".owner.json" }

// writeOwnerRecord persists the record beside the socket. Best-effort by
// design: failing to write provenance must not fail a spawn that succeeded —
// the daemon IS serving. Mirrors recordArenaConfig's placement: written only
// once the daemon is ready, so the record never describes a spawn that died.
func writeOwnerRecord(sockPath string, daemonPID int) {
	rec := OwnerRecord{
		Pin:        leylineBinaryVersion,
		DaemonPID:  daemonPID,
		SpawnerPID: os.Getpid(),
		StartedAt:  time.Now().UTC(),
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(ownerRecordPath(sockPath), data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "leyline: could not write owner record (provenance only, daemon unaffected): %v\n", err)
	}
}

// ReadOwnerRecord loads the record for a socket, reporting whether one exists
// and parses. Absence is normal — every daemon spawned before this existed,
// and daemons started by hand, have no record. A record proves provenance;
// its absence proves nothing.
func ReadOwnerRecord(sockPath string) (OwnerRecord, bool) {
	data, err := os.ReadFile(ownerRecordPath(sockPath))
	if err != nil {
		return OwnerRecord{}, false
	}
	var rec OwnerRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return OwnerRecord{}, false
	}
	return rec, true
}

// WellKnownOwnerRecord reads the record for the default shared socket.
// Exported for `mache doctor`, which reports provenance beside the daemon's
// self-reported version.
func WellKnownOwnerRecord() (OwnerRecord, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return OwnerRecord{}, false
	}
	return ReadOwnerRecord(home + "/.mache/default.sock")
}

// Orphaned reports whether the daemon has outlived its spawner: daemon alive,
// spawner dead. This is precisely the state that used to accumulate silently
// (the 11-day daemon), distinct from a STALE record where the daemon itself is
// gone and only the file remains.
func (r OwnerRecord) Orphaned() bool {
	return processAlive(r.DaemonPID) && !processAlive(r.SpawnerPID)
}

// Stale reports a record whose daemon is dead — the file outlived the process
// and should be reported rather than silently trusted.
func (r OwnerRecord) Stale() bool { return !processAlive(r.DaemonPID) }

// Age is how long the daemon has been running per the record.
func (r OwnerRecord) Age(now time.Time) time.Duration { return now.Sub(r.StartedAt) }
