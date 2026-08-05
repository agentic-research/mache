package leyline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// arenaSpawnConfig is the subset of daemon startup configuration that a warm
// arena cannot be silently reused across.
//
// The arena at ~/.mache/default.arena is a single fixed path shared by every
// project mache serves, but leyline's living database is bound to the tree it
// was parsed from: `verify_source_root_matches` (ley-line-open-c7d00f, present
// since v0.13.0 and so in mache's pinned binary) refuses to warm-start when the
// arena's recorded `_meta.source_root` disagrees with `--source`, with
//
//	arena source_root=<A> disagrees with --source=<B>. Refusing to warm-start;
//	the prior daemon's cached parses would silently pollute this run.
//
// Serving project B after project A therefore failed the daemon spawn outright,
// and because every DiscoverOrStart caller degrades rather than propagates
// (semantic_search, get_communities, the LSP handlers, validate, trigger), the
// symptom was daemon-backed features quietly going dark on the second project
// rather than a visible error.
//
// CDCTarget is carried for the same reason: switching CDC targets on a reused
// arena strands the previous target's manifest, and leyline's GC cannot reclaim
// it because those rows are fresh, not dead (ley-line-open-1869d0). Upstream
// chose to REPORT the stranded rows rather than reclaim them, and mache
// discards the daemon's stderr — so mache would never see that report. Cold
// starting instead of migrating sidesteps both halves.
type arenaSpawnConfig struct {
	// SourceRoot is the symlink-resolved --source directory, or "" when the
	// daemon is spawned without one (serving a pre-baked .db).
	SourceRoot string `json:"source_root"`
	// CDCTarget is the activation target the daemon was started with, or ""
	// when CDC is disabled. Today mache passes bare --cdc, whose target is
	// `nodes`; naming it here means flipping to `source-blobs` (mache-18caf3)
	// invalidates the arena automatically instead of stranding an index.
	CDCTarget string `json:"cdc_target"`
}

// arenaConfigPath is the sidecar next to the arena where mache records the
// configuration it last successfully spawned a managed daemon with. It sits
// beside the arena rather than inside it because leyline owns the arena's
// bytes; this file is mache's own bookkeeping.
func arenaConfigPath(arenaPath string) string {
	return arenaPath + ".mache-config.json"
}

// canonicalSourceRoot resolves dir the way leyline's own check does, so
// mache's reset decision and leyline's refusal decision agree on whether two
// paths name the same tree.
//
// EvalSymlinks (not just Abs) is load-bearing: leyline compares with Rust's
// `Path::canonicalize`, which resolves symlinks. This repo is routinely
// reached through both ~/github/art/mache and ~/remotes/art/mache — the same
// tree behind a symlink — and comparing unresolved paths would report a
// mismatch leyline itself would not, cold-starting on every alternation.
// A path that cannot be resolved (does not exist yet) falls back to Abs, and
// then to the input, so this never fails the spawn.
func canonicalSourceRoot(dir string) string {
	if dir == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

// arenaNeedsReset reports whether the daemon about to be spawned must be given
// --reset-arena, i.e. whether the arena on disk was last built for a different
// configuration than want.
//
// The rule is "reset unless we have a record that matches", not "reset when we
// have a record that differs". An arena with no record is one mache cannot
// vouch for — written by an older mache, by hand, or by a run that died before
// recording — and the cost of being wrong is asymmetric: a needless reset costs
// one cold parse, while a needless warm start is the hard spawn failure above.
// The first spawn after upgrading therefore cold-starts once, then steady state
// is warm.
func arenaNeedsReset(arenaPath string, want arenaSpawnConfig) bool {
	raw, err := os.ReadFile(arenaConfigPath(arenaPath))
	if err != nil {
		return true
	}
	var got arenaSpawnConfig
	if err := json.Unmarshal(raw, &got); err != nil {
		return true
	}
	return got != want
}

// resetArenaState invalidates a warm arena by removing the files leyline
// warm-starts from, so the next spawn cold-parses against the new --source.
//
// This does by hand what leyline's own `--reset-arena` flag advertises,
// because that flag does not work. Measured against the pinned v0.15.1
// binary, a daemon given --reset-arena fails on a COMPLETELY FRESH arena:
//
//	cold start (--reset-arena): unlinking any live db ... + zeroing controller
//	parsing /tmp/lz3/p1 ...
//	Error: open arena file
//	Caused by: 0: open arena file  1: No such file or directory (os error 2)
//
// The same command without the flag comes up cleanly. The cause is visible in
// cmd_daemon.rs: setup_arena stamps the controller with the real arena path,
// the reset branch then blanks it with set_arena_with_root("", 0, [0u8; 32]),
// and snapshot_to_arena reads that now-empty path back via ctrl.arena_path()
// into create_arena(...).context("open arena file"). The in-crate snapshot
// test only ever passes reset_arena: false, which is why it survived. The
// branch is byte-identical on the integration branch, so v0.18.0 will not fix
// it either — filed upstream.
//
// Removing the files reaches the same end state and is verified end-to-end:
// after it, a second project cold-starts and comes up, and a subsequent
// unchanged run still warm-starts rather than reparsing.
//
// Both warm-start sources must go. Removing only the living database leaves
// the arena itself carrying a snapshot root, and leyline refuses again one
// layer down — "warm start (arena)" rather than "warm start (live-db)".
//
// Errors are deliberately not propagated: a file that cannot be removed
// yields at worst the warm-start refusal this is trying to avoid, which is no
// worse than not having tried, and failing the spawn over a cleanup problem
// would be a strictly larger outage.
func resetArenaState(arenaPath, ctrlPath string) {
	paths := []string{
		arenaPath,
		arenaPath + ".owner", // source-root sentinel (arena_owner::try_acquire)
		arenaPath + ".lock",  // flock admission file
		ctrlPath,
		arenaConfigPath(arenaPath), // our own record; rewritten only on success
	}
	// The living database and its capnp sidecars are derived from the control
	// path's stem: <stem>.ctrl -> <stem>.live.db, plus -wal/-shm and
	// <stem>.live.{ast,head,source}.capnp. Glob the family rather than listing
	// the suffixes, which have grown over releases. The glob is anchored to
	// ".live." so a neighbouring file sharing the stem is never matched.
	stem := strings.TrimSuffix(ctrlPath, ".ctrl")
	if matches, err := filepath.Glob(stem + ".live.*"); err == nil {
		paths = append(paths, matches...)
	}
	for _, p := range paths {
		_ = os.Remove(p)
	}
}

// recordArenaConfig persists the configuration a managed daemon was
// successfully started with. Called only after the socket accepts a
// connection: recording before the spawn would claim an arena state that a
// crashed startup never actually produced, and the next run would warm-start
// onto it. A write failure is not fatal — it costs the next spawn a reset.
func recordArenaConfig(arenaPath string, cfg arenaSpawnConfig) error {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	// Write-then-rename so a crash mid-write cannot leave a truncated record.
	// A truncated record would merely fail to unmarshal and force a reset, but
	// atomicity is cheap enough that relying on that is not worth it.
	final := arenaConfigPath(arenaPath)
	tmp, err := os.CreateTemp(filepath.Dir(final), ".mache-config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, final); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}
