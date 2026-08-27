package graph

import (
	"database/sql"
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
func stalenessFixture(t *testing.T, builtAt time.Time) (*SQLiteGraph, string) {
	t.Helper()
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
