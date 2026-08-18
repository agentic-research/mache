package leyline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArenaNeedsReset_NoRecordMeansReset(t *testing.T) {
	arena := filepath.Join(t.TempDir(), "default.arena")
	assert.True(t, arenaNeedsReset(arena, arenaSpawnConfig{SourceRoot: "/x"}),
		"an arena mache has no record for cannot be vouched for; a needless cold parse beats leyline's warm-start refusal")
}

func TestArenaNeedsReset_MatchingRecordWarmStarts(t *testing.T) {
	arena := filepath.Join(t.TempDir(), "default.arena")
	cfg := arenaSpawnConfig{SourceRoot: "/projects/alpha", CDCTarget: ""}
	require.NoError(t, recordArenaConfig(arena, cfg))

	assert.False(t, arenaNeedsReset(arena, cfg),
		"an unchanged configuration must warm-start — resetting every spawn would reparse the whole tree each time")
}

// TestArenaNeedsReset_ChangedFieldResets is the regression this whole file
// exists for. Serving a second project from mache's one fixed arena made
// leyline refuse to warm-start:
//
//	arena source_root=/private/tmp/lz/p1 disagrees with --source=/private/tmp/lz/p2.
//	Refusing to warm-start ...
//
// reproduced directly against the pinned v0.15.1 binary. The CDC case is the
// same shape: a target switch strands the previous target's manifest, which
// leyline reports but cannot reclaim (ley-line-open-1869d0).
func TestArenaNeedsReset_ChangedFieldResets(t *testing.T) {
	recorded := arenaSpawnConfig{SourceRoot: "/projects/alpha", CDCTarget: "nodes"}
	cases := map[string]arenaSpawnConfig{
		"different project":  {SourceRoot: "/projects/beta", CDCTarget: "nodes"},
		"cdc target flipped": {SourceRoot: "/projects/alpha", CDCTarget: "source-blobs"},
		"cdc disabled":       {SourceRoot: "/projects/alpha", CDCTarget: ""},
		"source dropped":     {SourceRoot: "", CDCTarget: "nodes"},
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			arena := filepath.Join(t.TempDir(), "default.arena")
			require.NoError(t, recordArenaConfig(arena, recorded))
			assert.True(t, arenaNeedsReset(arena, want),
				"a changed spawn configuration must invalidate the arena")
		})
	}
}

func TestArenaNeedsReset_CorruptRecordResets(t *testing.T) {
	arena := filepath.Join(t.TempDir(), "default.arena")
	require.NoError(t, os.WriteFile(arenaConfigPath(arena), []byte("{not json"), 0o644))

	assert.True(t, arenaNeedsReset(arena, arenaSpawnConfig{SourceRoot: "/x"}),
		"an unparseable record is not a match; it must not be read as agreement")
}

func TestRecordArenaConfig_OverwritesPriorRecord(t *testing.T) {
	arena := filepath.Join(t.TempDir(), "default.arena")
	require.NoError(t, recordArenaConfig(arena, arenaSpawnConfig{SourceRoot: "/projects/alpha"}))
	newCfg := arenaSpawnConfig{SourceRoot: "/projects/beta", CDCTarget: "source-blobs"}
	require.NoError(t, recordArenaConfig(arena, newCfg))

	raw, err := os.ReadFile(arenaConfigPath(arena))
	require.NoError(t, err)
	var got arenaSpawnConfig
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, newCfg, got, "the record must describe the LAST successful spawn, not the first")
	assert.False(t, arenaNeedsReset(arena, newCfg))
}

// TestRecordArenaConfig_LeavesNoTempFiles guards the write-then-rename: a
// leaked .tmp per spawn would accumulate in the user's ~/.mache forever.
func TestRecordArenaConfig_LeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	arena := filepath.Join(dir, "default.arena")
	require.NoError(t, recordArenaConfig(arena, arenaSpawnConfig{SourceRoot: "/x"}))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.Equal(t, []string{filepath.Base(arenaConfigPath(arena))}, names,
		"only the final record may remain; a leaked temp file per spawn accumulates in ~/.mache")
}

// TestCanonicalSourceRoot_ResolvesSymlinks is the case that would otherwise
// cold-start on every alternation: this repo is reached through both
// ~/github/art/mache and ~/remotes/art/mache, the same tree behind a symlink.
// leyline compares with Rust's Path::canonicalize, which resolves symlinks, so
// mache must too or it would reset for a mismatch leyline itself would not see.
func TestCanonicalSourceRoot_ResolvesSymlinks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real-tree")
	require.NoError(t, os.MkdirAll(real, 0o755))
	link := filepath.Join(dir, "link-to-tree")
	require.NoError(t, os.Symlink(real, link))

	assert.Equal(t, CanonicalSourceRoot(real), CanonicalSourceRoot(link),
		"a symlink and its target name the same tree and must not invalidate each other's arena")
}

func TestCanonicalSourceRoot_EmptyStaysEmpty(t *testing.T) {
	assert.Empty(t, CanonicalSourceRoot(""),
		`a daemon spawned without --source records "", not the process working directory`)
}

func TestCanonicalSourceRoot_NonexistentPathIsStillAbsolute(t *testing.T) {
	got := CanonicalSourceRoot("relative/not/created")
	assert.True(t, filepath.IsAbs(got),
		"an unresolvable path must still normalize to absolute rather than fail the spawn")
}

// TestInspectArena_ReportsEveryStateRatherThanErroring pins the contract that
// makes InspectArena usable from a diagnostic: a missing arena, a missing
// record, and an unparseable record are all STATES to describe, not errors to
// propagate. A diagnostic that errors out on "nothing configured yet" cannot
// report on a machine that has never run mache — which is exactly the machine
// most likely to need it.
func TestInspectArena_ReportsEveryStateRatherThanErroring(t *testing.T) {
	t.Run("nothing present", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		st, err := InspectArena()
		require.NoError(t, err)
		assert.False(t, st.Exists)
		assert.False(t, st.HasRecord)
		assert.NotEmpty(t, st.Path, "the path must be reported even when the file is absent")
		assert.Equal(t, arenaConfigPath(st.Path), st.ConfigPath)
	})

	t.Run("arena without a record", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		require.NoError(t, os.MkdirAll(filepath.Join(home, ".mache"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(home, ".mache", "default.arena"), []byte("x"), 0o644))

		st, err := InspectArena()
		require.NoError(t, err)
		assert.True(t, st.Exists)
		assert.False(t, st.HasRecord, "an arena mache never recorded cannot be vouched for")
	})

	t.Run("unparseable record reads as absent, not as agreement", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		require.NoError(t, os.MkdirAll(filepath.Join(home, ".mache"), 0o700))
		arena := filepath.Join(home, ".mache", "default.arena")
		require.NoError(t, os.WriteFile(arena, []byte("x"), 0o644))
		require.NoError(t, os.WriteFile(arenaConfigPath(arena), []byte("{not json"), 0o644))

		st, err := InspectArena()
		require.NoError(t, err)
		assert.False(t, st.HasRecord, "corrupt bookkeeping must never be read as a match")
	})

	t.Run("intact record round-trips", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		require.NoError(t, os.MkdirAll(filepath.Join(home, ".mache"), 0o700))
		arena := filepath.Join(home, ".mache", "default.arena")
		require.NoError(t, os.WriteFile(arena, []byte("x"), 0o644))
		require.NoError(t, recordArenaConfig(arena, arenaSpawnConfig{SourceRoot: "/projects/alpha", CDCTarget: "source-blobs"}))

		st, err := InspectArena()
		require.NoError(t, err)
		require.True(t, st.HasRecord)
		assert.Equal(t, "/projects/alpha", st.SourceRoot)
		assert.Equal(t, "source-blobs", st.CDCTarget)
	})
}

// TestArenaBoundElsewhere_UnknownIsNotDisagreement is the guard against a
// diagnostic that reports a conflict it cannot actually observe. Without a
// record, or without a candidate, there is nothing to disagree with — and
// claiming otherwise would send an operator chasing a warm-start refusal that
// ley-line is not going to issue.
func TestArenaBoundElsewhere_UnknownIsNotDisagreement(t *testing.T) {
	assert.False(t, ArenaState{}.ArenaBoundElsewhere("/anywhere"),
		"no record means unknown, and unknown is not disagreement")
	assert.False(t, ArenaState{HasRecord: true}.ArenaBoundElsewhere("/anywhere"),
		"an empty recorded source_root is 'bound to no tree', not a conflict")
	assert.False(t, ArenaState{HasRecord: true, SourceRoot: "/a"}.ArenaBoundElsewhere(""),
		"no candidate to compare against is not a conflict either")
}

// TestArenaBoundElsewhere_CanonicalizesBothSides is the symlink case that would
// otherwise report a mismatch ley-line itself would never see: this repo is
// reached through both ~/github/art/mache and ~/remotes/art/mache.
func TestArenaBoundElsewhere_CanonicalizesBothSides(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "tree")
	require.NoError(t, os.MkdirAll(real, 0o755))
	link := filepath.Join(dir, "link")
	require.NoError(t, os.Symlink(real, link))

	st := ArenaState{HasRecord: true, SourceRoot: real}
	assert.False(t, st.ArenaBoundElsewhere(link),
		"a symlink and its target name the same tree; reporting a conflict would be a false alarm")

	other := filepath.Join(dir, "elsewhere")
	require.NoError(t, os.MkdirAll(other, 0o755))
	assert.True(t, st.ArenaBoundElsewhere(other),
		"a genuinely different tree must still be reported")
}
