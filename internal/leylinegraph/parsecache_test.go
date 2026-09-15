package leylinegraph

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentic-research/mache/internal/leyline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// useTempParseCache points the parse cache at a temp dir for the duration of a
// test. Named for what it does rather than what it returns, so it does not
// collide with internal/leyline's cacheDir (the pinned-binary location).
func useTempParseCache(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(ParseCacheDirEnv, dir)
	return dir
}

// TestReserveParseCache_StablePerSource is the property the whole speedup
// rests on: the SAME source dir must get the SAME db path on every run.
// leyline re-parses only what changed by diffing against what the db already
// holds, so a path that varies per invocation — which is what os.CreateTemp
// gave it — means it has nothing to diff and re-parses everything
// (mache-80a851).
func TestReserveParseCache_StablePerSource(t *testing.T) {
	useTempParseCache(t)
	src := t.TempDir()

	first := reserveParseCache(src)
	require.NotNil(t, first)
	path := first.path
	first.release()

	second := reserveParseCache(src)
	require.NotNil(t, second)
	defer second.release()
	assert.Equal(t, path, second.path, "the same source must reuse the same parse db")
}

// TestReserveParseCache_KeyedPerProject: one shared db for every project is
// the failure mache-e3e19f records — a second tree hits leyline's warm-start
// refusal and daemon-backed features go dark.
func TestReserveParseCache_KeyedPerProject(t *testing.T) {
	useTempParseCache(t)

	a := reserveParseCache(t.TempDir())
	require.NotNil(t, a)
	defer a.release()
	b := reserveParseCache(t.TempDir())
	require.NotNil(t, b)
	defer b.release()

	assert.NotEqual(t, a.path, b.path, "two source trees must not share one parse db")
}

// TestReserveParseCache_KeyedPerLeylineVersion: a different leyline writes a
// different `_ast` schema, so a db produced by another version has to MISS
// rather than be read with the wrong shape.
func TestReserveParseCache_KeyedPerLeylineVersion(t *testing.T) {
	useTempParseCache(t)
	entry := reserveParseCache(t.TempDir())
	require.NotNil(t, entry)
	defer entry.release()

	assert.Contains(t, filepath.Base(entry.path), leyline.BinaryVersion,
		"the pinned leyline version must be part of the cache key")
}

// TestReserveParseCache_SecondHolderFallsBack pins the concurrency contract.
// A second mache on the same project must NOT re-parse into a db the first is
// serving from; it takes the temp path instead, which is the isolation every
// process had before this cache existed.
func TestReserveParseCache_SecondHolderFallsBack(t *testing.T) {
	useTempParseCache(t)
	src := t.TempDir()

	held := reserveParseCache(src)
	require.NotNil(t, held)

	assert.Nil(t, reserveParseCache(src),
		"a second holder got the same db while the first still had it open")

	// Releasing hands it back.
	held.release()
	after := reserveParseCache(src)
	require.NotNil(t, after, "the db stayed locked after release")
	after.release()
}

// TestParseCacheEntry_ReleaseKeepsDB_DiscardRemovesIt: persisting the db
// across runs IS the feature, so release must not delete it. discard must,
// including the capnp sidecars leyline writes beside it — they share the db's
// STEM, not its full name, so a naive `path + suffix` sweep leaves them to be
// read against a db that no longer matches.
func TestParseCacheEntry_ReleaseKeepsDB_DiscardRemovesIt(t *testing.T) {
	useTempParseCache(t)
	src := t.TempDir()

	entry := reserveParseCache(src)
	require.NotNil(t, entry)
	stem := strings.TrimSuffix(entry.path, ".db")
	sidecars := []string{stem + ".ast.capnp", stem + ".head.capnp", stem + ".source.capnp"}
	for _, f := range append([]string{entry.path}, sidecars...) {
		require.NoError(t, os.WriteFile(f, []byte("x"), 0o600))
	}

	entry.release()
	_, err := os.Stat(entry.path)
	require.NoError(t, err, "release deleted the db; persisting it across runs is the point")

	again := reserveParseCache(src)
	require.NotNil(t, again)
	again.discard()
	for _, f := range append([]string{entry.path}, sidecars...) {
		_, statErr := os.Stat(f)
		assert.True(t, os.IsNotExist(statErr), "discard left %s behind", filepath.Base(f))
	}
}

// TestParseCacheDir_UnavailableWithoutOverride: with no override and the real
// HOME, projcfg's test-hermeticity guard refuses, the cache is unavailable,
// and the caller silently gets the pre-existing temp-file behaviour. A test
// can therefore never populate a developer's real ~/.mache (mache-3e78d2).
func TestParseCacheDir_UnavailableWithoutOverride(t *testing.T) {
	t.Setenv(ParseCacheDirEnv, "")
	_, err := parseCacheDir()
	require.Error(t, err, "a test reached the real ~/.mache parse cache")
	assert.Nil(t, reserveParseCache(t.TempDir()),
		"an unavailable cache must report unavailable, not panic or half-reserve")
}
