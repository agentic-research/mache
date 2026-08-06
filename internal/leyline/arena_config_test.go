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

	assert.Equal(t, canonicalSourceRoot(real), canonicalSourceRoot(link),
		"a symlink and its target name the same tree and must not invalidate each other's arena")
}

// TestResetArenaState_RemovesBothWarmStartSources pins the two-layer lesson
// measured against the pinned binary: removing only the living database is not
// enough, because the arena file carries a snapshot root of its own and
// leyline refuses one layer down with "warm start (arena)" instead of
// "warm start (live-db)".
func TestResetArenaState_RemovesBothWarmStartSources(t *testing.T) {
	dir := t.TempDir()
	arena := filepath.Join(dir, "default.arena")
	ctrl := filepath.Join(dir, "default.ctrl")
	stale := []string{
		"default.arena", "default.arena.owner", "default.arena.lock", "default.ctrl",
		"default.live.db", "default.live.db-wal", "default.live.db-shm",
		"default.live.ast.capnp", "default.live.head.capnp", "default.live.source.capnp",
	}
	for _, name := range stale {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("stale"), 0o644))
	}
	require.NoError(t, recordArenaConfig(arena, arenaSpawnConfig{SourceRoot: "/projects/alpha"}))

	resetArenaState(arena, ctrl)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries,
		"every warm-start source, the owner/lock sentinels, and our own stale record must go")
}

// TestResetArenaState_LeavesUnrelatedNeighboursAlone guards the glob. The real
// ~/.mache holds a plain file literally named "default" next to
// default.arena; a stem glob that matched it would delete unrelated state.
func TestResetArenaState_LeavesUnrelatedNeighboursAlone(t *testing.T) {
	dir := t.TempDir()
	keep := []string{"default", "default.livewire", "other.live.db", "default.arena.bak"}
	for _, name := range keep {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("keep"), 0o644))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "default.live.db"), []byte("stale"), 0o644))

	resetArenaState(filepath.Join(dir, "default.arena"), filepath.Join(dir, "default.ctrl"))

	for _, name := range keep {
		assert.FileExists(t, filepath.Join(dir, name), "%s is not arena state and must survive", name)
	}
	assert.NoFileExists(t, filepath.Join(dir, "default.live.db"))
}

// TestResetArenaState_MissingFilesAreNotAnError covers the first-ever spawn:
// nothing exists yet, and invalidation must be a silent no-op rather than a
// failure that takes the daemon down with it.
func TestResetArenaState_MissingFilesAreNotAnError(t *testing.T) {
	dir := t.TempDir()
	assert.NotPanics(t, func() {
		resetArenaState(filepath.Join(dir, "default.arena"), filepath.Join(dir, "default.ctrl"))
	})
}

func TestCanonicalSourceRoot_EmptyStaysEmpty(t *testing.T) {
	assert.Empty(t, canonicalSourceRoot(""),
		`a daemon spawned without --source records "", not the process working directory`)
}

func TestCanonicalSourceRoot_NonexistentPathIsStillAbsolute(t *testing.T) {
	got := canonicalSourceRoot("relative/not/created")
	assert.True(t, filepath.IsAbs(got),
		"an unresolvable path must still normalize to absolute rather than fail the spawn")
}
