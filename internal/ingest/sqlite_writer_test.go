package ingest

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// LoadFileIndex is the read-side of incremental re-ingestion: when
// `mache build` runs over a tree we've seen before, it loads this
// table to skip files whose (path, mod_time, size) tuple is
// unchanged. A wrong answer here = unnecessary re-parse of the
// whole tree (slow but correct), or worse, files silently skipped
// when they shouldn't be (fast but stale). Direct tests pin the
// three cases callers can hit:
//
//   - Happy path: rows present → map populated correctly
//   - Empty DB: no file_index table → nil, nil (caller treats as
//     "no cache, ingest everything")
//   - Missing DB file: returns error
//
// The InsertOrReplace round-trip is exercised by callers that
// build the index during ingestion; here we focus on the load.

func writeFileIndexRow(t *testing.T, dbPath, path string, modTime time.Time, size int64) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS file_index (
		path TEXT PRIMARY KEY,
		mod_time INTEGER NOT NULL,
		size INTEGER NOT NULL
	)`)
	require.NoError(t, err)

	_, err = db.Exec(
		"INSERT OR REPLACE INTO file_index (path, mod_time, size) VALUES (?, ?, ?)",
		path, modTime.UnixNano(), size,
	)
	require.NoError(t, err)
}

func TestLoadFileIndex_PopulatedTable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "index.db")
	t1 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 1, 12, 30, 0, 0, time.UTC)

	writeFileIndexRow(t, dbPath, "main.go", t1, 1024)
	writeFileIndexRow(t, dbPath, "lib/util.go", t2, 2048)

	idx, err := LoadFileIndex(dbPath)
	require.NoError(t, err)
	require.Len(t, idx, 2)

	main, ok := idx["main.go"]
	require.True(t, ok, "main.go entry should be present")
	assert.Equal(t, int64(1024), main.Size)
	assert.True(t, main.ModTime.Equal(t1), "ModTime must round-trip via UnixNano")

	util, ok := idx["lib/util.go"]
	require.True(t, ok, "lib/util.go entry should be present")
	assert.Equal(t, int64(2048), util.Size)
	assert.True(t, util.ModTime.Equal(t2))
}

func TestLoadFileIndex_NoTableReturnsNilNil(t *testing.T) {
	// A fresh .db with no file_index table is the expected state for
	// first-ever-builds. The caller (`mache build` incremental path)
	// distinguishes "no cache yet" from "load failed" by the error;
	// nil-map + nil-err is the contract.
	dbPath := filepath.Join(t.TempDir(), "empty.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	idx, err := LoadFileIndex(dbPath)
	require.NoError(t, err, "missing file_index table is not an error")
	assert.Nil(t, idx, "no cache → nil map (caller treats as 'ingest everything')")
}

func TestLoadFileIndex_NonexistentDBReturnsError(t *testing.T) {
	idx, err := LoadFileIndex("/nonexistent/path/to/db.sqlite")
	require.Error(t, err, "missing db file must surface as error, not silent nil")
	assert.Nil(t, idx)
}

func TestLoadFileIndex_EmptyTableReturnsEmptyMap(t *testing.T) {
	// Distinct from the "no table" case: file_index exists but has
	// zero rows. Returns a non-nil empty map so callers can iterate
	// safely (range over nil is OK in Go but len(nil)==0 reads
	// awkwardly elsewhere).
	dbPath := filepath.Join(t.TempDir(), "empty_table.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE file_index (
		path TEXT PRIMARY KEY,
		mod_time INTEGER NOT NULL,
		size INTEGER NOT NULL
	)`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	idx, err := LoadFileIndex(dbPath)
	require.NoError(t, err)
	require.NotNil(t, idx)
	assert.Empty(t, idx)
}
