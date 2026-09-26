package leylinegraph

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeEntry lays down a db plus the sidecars leyline writes beside it, with
// a given age. Sizes differ per file so a test can tell which entry was freed.
func writeEntry(t *testing.T, dir, base string, age time.Duration, dbSize, sidecarSize int) string {
	t.Helper()
	db := filepath.Join(dir, base+".db")
	stem := filepath.Join(dir, base)
	files := map[string]int{
		db:                     dbSize,
		db + "-wal":            8,
		stem + ".ast.capnp":    sidecarSize,
		stem + ".source.capnp": 8,
	}
	for path, size := range files {
		require.NoError(t, os.WriteFile(path, make([]byte, size), 0o600))
	}
	when := time.Now().Add(-age)
	for path := range files {
		require.NoError(t, os.Chtimes(path, when, when))
	}
	return db
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

func TestReapStaleTempDBs_RemovesOnlyTheOldOnes(t *testing.T) {
	dir := t.TempDir()
	old := writeEntry(t, dir, "mache-leyline-111", 48*time.Hour, 16, 16)
	fresh := writeEntry(t, dir, "mache-leyline-222", time.Minute, 16, 16)
	// Not ours. A reaper that globs too widely eats someone else's data.
	other := writeEntry(t, dir, "postgres-backup-333", 48*time.Hour, 16, 16)

	n := reapStaleTempDBs(dir, 24*time.Hour, time.Now())

	assert.Equal(t, 1, n)
	assert.False(t, exists(t, old), "a day-old orphan must be reaped")
	assert.True(t, exists(t, fresh), "a parse from a minute ago may still be running")
	assert.True(t, exists(t, other), "only mache-leyline-*.db is ours to delete")
}

// The sidecars share the db's STEM, not its name, so a reaper that removes
// `path + suffix` leaves the largest file of the entry behind — and the
// .ast.capnp is routinely bigger than the .db it belongs to.
func TestReapStaleTempDBs_RemovesTheSidecarsToo(t *testing.T) {
	dir := t.TempDir()
	db := writeEntry(t, dir, "mache-leyline-444", 48*time.Hour, 16, 4096)

	reapStaleTempDBs(dir, 24*time.Hour, time.Now())

	// Spelled out rather than looped over entryFiles: asserting against the
	// very function under test passes vacuously when that function stops
	// listing a file, which is exactly the defect this guards.
	stem := strings.TrimSuffix(db, ".db")
	for _, f := range []string{
		db, db + "-wal", stem + ".ast.capnp", stem + ".source.capnp",
	} {
		assert.False(t, exists(t, f), "%s survived the reap", filepath.Base(f))
	}
}

func TestEvictParseCache_EvictsLeastRecentlyUsedUntilUnderBudget(t *testing.T) {
	dir := t.TempDir()
	oldest := writeEntry(t, dir, "aaaa-v1", 72*time.Hour, 400, 400)
	middle := writeEntry(t, dir, "bbbb-v1", 48*time.Hour, 400, 400)
	newest := writeEntry(t, dir, "cccc-v1", time.Minute, 400, 400)

	// Each entry is ~816 B, so a 1 KiB budget fits exactly one.
	freed := evictParseCache(dir, 1024)

	assert.Positive(t, freed)
	assert.False(t, exists(t, oldest), "the least recently used entry goes first")
	assert.False(t, exists(t, middle))
	assert.True(t, exists(t, newest), "eviction must stop once it is under budget")
}

func TestEvictParseCache_UnderBudgetIsANoOp(t *testing.T) {
	dir := t.TempDir()
	keep := writeEntry(t, dir, "aaaa-v1", 72*time.Hour, 100, 100)

	assert.Zero(t, evictParseCache(dir, 10<<20))
	assert.True(t, exists(t, keep), "nothing may be evicted while the cache fits")
}

// An entry another mache is serving from must survive, even when it is the
// least recently used and the cache is over budget. Deleting it would pull the
// db out from under a live reader.
func TestEvictParseCache_SkipsAnEntryAnotherProcessHolds(t *testing.T) {
	dir := t.TempDir()
	held := writeEntry(t, dir, "aaaa-v1", 72*time.Hour, 400, 400)
	free := writeEntry(t, dir, "bbbb-v1", 48*time.Hour, 400, 400)

	lock, err := os.OpenFile(held+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	require.NoError(t, err)
	require.NoError(t, syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
	defer func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}()

	evictParseCache(dir, 1024)

	assert.True(t, exists(t, held), "a locked entry is in use and must not be evicted")
	assert.False(t, exists(t, free), "the unlocked entry is what pays for the budget")
}

// A budget that nothing can satisfy must terminate rather than spin: with
// every entry locked there is no space to reclaim, and the loop has to notice.
func TestEvictParseCache_TerminatesWhenEverythingIsPinned(t *testing.T) {
	dir := t.TempDir()
	a := writeEntry(t, dir, "aaaa-v1", 72*time.Hour, 400, 400)
	b := writeEntry(t, dir, "bbbb-v1", 48*time.Hour, 400, 400)

	var locks []*os.File
	for _, db := range []string{a, b} {
		lock, err := os.OpenFile(db+".lock", os.O_CREATE|os.O_RDWR, 0o600)
		require.NoError(t, err)
		require.NoError(t, syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
		locks = append(locks, lock)
	}
	defer func() {
		for _, l := range locks {
			_ = syscall.Flock(int(l.Fd()), syscall.LOCK_UN)
			_ = l.Close()
		}
	}()

	// Called directly rather than raced against a timer: `go test` already
	// has a timeout, and it reports a hang as the hang it is. A time.After
	// here would be a second, weaker deadline — and a timing primitive in a
	// test, which the sleep_in_test rule flags for good reason.
	assert.Zero(t, evictParseCache(dir, 1), "nothing was evictable")
	assert.True(t, exists(t, a))
	assert.True(t, exists(t, b))
}

func TestEntryBytes_CountsTheSidecars(t *testing.T) {
	dir := t.TempDir()
	db := writeEntry(t, dir, "aaaa-v1", time.Minute, 100, 4096)

	// 100 (db) + 8 (-wal) + 4096 (.ast.capnp) + 8 (.source.capnp).
	assert.Equal(t, int64(100+8+4096+8), entryBytes(db),
		"budgeting on the .db alone undercounts an entry by most of its size")
}
