package graph

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// stalenessFixture builds a minimal .db carrying _meta.source_root, returns the
// graph and the source dir. The db's file mtime is set explicitly so tests
// control "when the index was built" without sleeping.
func stalenessFixture(t *testing.T, builtAt time.Time, indexed ...string) (*SQLiteGraph, string) {
	t.Helper()
	src := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "g.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE _meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
		INSERT INTO _meta VALUES ('source_root', '` + src + `');
		CREATE TABLE nodes (id TEXT PRIMARY KEY, parent_id TEXT, name TEXT NOT NULL,
			kind INTEGER NOT NULL, size INTEGER DEFAULT 0, mtime INTEGER NOT NULL, record TEXT,
			source_file TEXT)`)
	require.NoError(t, err)
	for i, rel := range indexed {
		_, err = db.Exec(`INSERT INTO nodes (id, name, kind, mtime, source_file) VALUES (?, ?, 1, 0, ?)`,
			fmt.Sprintf("n%d", i), rel, rel)
		require.NoError(t, err)
	}
	require.NoError(t, db.Close())
	require.NoError(t, os.Chtimes(dbPath, builtAt, builtAt))

	g, err := Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })
	return g, src
}

// touch writes a file with an explicit mtime.
func touch(t *testing.T, dir, name string, mtime time.Time) {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte("x"), 0o644))
	require.NoError(t, os.Chtimes(p, mtime, mtime))
}

// TestIndexStaleness_CountsOnlyEditsAfterTheBuild pins the core derivation:
// the .db file's own mtime IS the build time, _meta.source_root IS the tree,
// and only files modified AFTER the build count. Nothing is stamped anywhere —
// a pure read-side derivation, which is what makes it work retroactively on
// every .db that already exists.
func TestIndexStaleness_CountsOnlyEditsAfterTheBuild(t *testing.T) {
	built := time.Now().Add(-time.Hour)
	g, src := stalenessFixture(t, built)

	touch(t, src, "old.go", built.Add(-time.Minute))    // pre-build: not drift
	touch(t, src, "edited.go", built.Add(time.Minute))  // post-build: drift
	touch(t, src, "sub/new.go", built.Add(time.Minute)) // nested drift counts

	rep, ok := g.IndexStaleness()
	require.True(t, ok)
	assert.Equal(t, 2, rep.ModifiedSince,
		"pre-build files are IN the index; only post-build edits are drift")
	assert.Equal(t, src, rep.SourceRoot)
	assert.WithinDuration(t, built, rep.BuiltAt, time.Second)
	assert.False(t, rep.Capped)
}

// TestIndexStaleness_NoiseDirsAndDotfilesDoNotCount: .git churns on every
// command without changing what the index should contain — counting it would
// cry wolf on every overview, which trains agents to ignore the one warning
// that matters.
func TestIndexStaleness_NoiseDirsAndDotfilesDoNotCount(t *testing.T) {
	built := time.Now().Add(-time.Hour)
	g, src := stalenessFixture(t, built)

	touch(t, src, ".git/index", built.Add(time.Minute))
	touch(t, src, "node_modules/x/y.js", built.Add(time.Minute))
	touch(t, src, ".hidden", built.Add(time.Minute))

	rep, ok := g.IndexStaleness()
	require.True(t, ok)
	assert.Zero(t, rep.ModifiedSince)
}

// TestIndexStaleness_UnknownIsNotFresh pins the failure direction: a graph
// that cannot answer (no _meta, or the source tree is gone) reports ok=false,
// and the overview OMITS the block. Omission must mean "unknown" — reporting
// zero drift for an unanswerable graph would be the freshness version of the
// success-claim lie.
func TestIndexStaleness_UnknownIsNotFresh(t *testing.T) {
	t.Run("no _meta table", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "g.db")
		db, err := sql.Open("sqlite", dbPath)
		require.NoError(t, err)
		_, err = db.Exec(`CREATE TABLE nodes (id TEXT PRIMARY KEY, parent_id TEXT, name TEXT NOT NULL,
			kind INTEGER NOT NULL, size INTEGER DEFAULT 0, mtime INTEGER NOT NULL, record TEXT)`)
		require.NoError(t, err)
		require.NoError(t, db.Close())
		g, err := Open(dbPath)
		require.NoError(t, err)
		t.Cleanup(func() { _ = g.Close() })

		_, ok := g.IndexStaleness()
		assert.False(t, ok)
	})
	t.Run("source tree deleted", func(t *testing.T) {
		g, src := stalenessFixture(t, time.Now())
		require.NoError(t, os.RemoveAll(src))
		_, ok := g.IndexStaleness()
		assert.False(t, ok, "a vanished tree is unknown, not zero-drift")
	})
}

// TestIndexStaleness_CapStopsTheWalk: past the cap the answer is "a lot", and
// the walk must stop rather than pay for an exact number nobody acts on.
func TestIndexStaleness_CapStopsTheWalk(t *testing.T) {
	built := time.Now().Add(-time.Hour)
	g, src := stalenessFixture(t, built)
	for i := range staleScanCap + 50 {
		touch(t, src, filepath.Join("d", "f"+string(rune('a'+i%26))+string(rune('0'+i%10))+string(rune('0'+(i/10)%10))+string(rune('0'+(i/100)%10))+".go"), built.Add(time.Minute))
	}

	rep, ok := g.IndexStaleness()
	require.True(t, ok)
	assert.Equal(t, staleScanCap, rep.ModifiedSince)
	assert.True(t, rep.Capped, "the report must say the true number is larger")
}

// TestIndexStaleness_DetectsDeletedIndexedFiles pins the deletion half of
// drift, which the mtime walk is structurally blind to: a deleted file is
// not in the walk, and a rename preserves mtimes, so before DeletedSince a
// rename was DOUBLY invisible. Detection is exact — the .db records every
// file it indexed, so indexed-but-missing is a fact, never a dir-mtime guess.
func TestIndexStaleness_DetectsDeletedIndexedFiles(t *testing.T) {
	built := time.Now().Add(-time.Hour)
	g, src := stalenessFixture(t, built, "keep.go", "gone.go", "sub/renamed.go")

	touch(t, src, "keep.go", built.Add(-time.Minute))
	// gone.go: indexed, never present on disk — reads as deleted.
	// sub/renamed.go: simulate `mv` — create the NEW name with a PRE-build
	// mtime (mv preserves mtimes), old indexed name absent.
	touch(t, src, "sub/newname.go", built.Add(-time.Minute))

	rep, ok := g.IndexStaleness()
	require.True(t, ok)
	assert.Equal(t, 2, rep.DeletedSince,
		"gone.go and the renamed-away sub/renamed.go are both indexed-but-missing")
	assert.Zero(t, rep.ModifiedSince,
		"the rename's new name carries a pre-build mtime — the walk alone sees NOTHING, which is why DeletedSince exists")
}

// TestIndexStaleness_NoSourceFileColumnStillReports pins graceful degradation:
// an older projection without nodes.source_file reports modifications as
// before and simply omits deletion detection — a partial answer, not a
// refusal.
func TestIndexStaleness_NoSourceFileColumnStillReports(t *testing.T) {
	built := time.Now().Add(-time.Hour)
	src := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "g.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE _meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
		INSERT INTO _meta VALUES ('source_root', '` + src + `');
		CREATE TABLE nodes (id TEXT PRIMARY KEY, parent_id TEXT, name TEXT NOT NULL,
			kind INTEGER NOT NULL, size INTEGER DEFAULT 0, mtime INTEGER NOT NULL, record TEXT)`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	require.NoError(t, os.Chtimes(dbPath, built, built))
	g, err := Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })

	touch(t, src, "edited.go", built.Add(time.Minute))
	rep, ok := g.IndexStaleness()
	require.True(t, ok)
	assert.Equal(t, 1, rep.ModifiedSince)
	assert.Zero(t, rep.DeletedSince)
}

// TestIndexStaleness_PrefersParseTimeStamp pins the BuiltAt upgrade: when the
// producer stamped _meta.parse_time, that beats the db file's mtime — a
// copied or transported .db keeps its true build time.
func TestIndexStaleness_PrefersParseTimeStamp(t *testing.T) {
	trueBuild := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	g, src := stalenessFixture(t, time.Now()) // file mtime says "just now"
	_, err := g.db.Exec(`INSERT INTO _meta VALUES ('parse_time', ?)`, trueBuild.Unix())
	require.NoError(t, err)

	// Edited between the TRUE build and the (wrong) file mtime: only the
	// parse_time derivation counts it.
	touch(t, src, "edited.go", trueBuild.Add(time.Minute))

	rep, ok := g.IndexStaleness()
	require.True(t, ok)
	assert.Equal(t, trueBuild.Unix(), rep.BuiltAt.Unix(), "parse_time must win over file mtime")
	assert.Equal(t, 1, rep.ModifiedSince)
}

// TestIndexStaleness_DeletionCapStopsTheScan mirrors the walk's cap: past
// staleScanCap missing files the answer is "a lot", reported as a floor.
func TestIndexStaleness_DeletionCapStopsTheScan(t *testing.T) {
	built := time.Now().Add(-time.Hour)
	indexed := make([]string, staleScanCap+50)
	for i := range indexed {
		indexed[i] = fmt.Sprintf("gone%04d.go", i)
	}
	g, _ := stalenessFixture(t, built, indexed...)

	rep, ok := g.IndexStaleness()
	require.True(t, ok)
	assert.Equal(t, staleScanCap, rep.DeletedSince)
	assert.True(t, rep.Capped, "the report must say the true number is larger")
}
