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

// _index_coverage exists per ADR-0013 Step 2 (mache-346cfe). Tests
// pin the producer-agnostic write path, the round-trip through
// LoadIndexCoverage, and the contract that distinguishes "table
// missing" (nil) from "table present but empty" (non-nil empty map).
//
// Consumers that want to know whether a source was indexed at a
// given fidelity rely on these distinctions per the ADR's wedge
// case 1 ("LSP coverage is partial, not total"): "no coverage row
// exists" must be unambiguous, never confused with "the consumer
// didn't look."

func TestSQLiteWriter_RecordIndexCoverage_RoundTrip(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "coverage.db")
	w, err := NewSQLiteWriter(dbPath)
	require.NoError(t, err)

	// Two producers cover the same source at different fidelities;
	// a third source has only tree-sitter coverage. PRIMARY KEY
	// (source_id, producer) means each (source, producer) pair has
	// at most one row.
	t1 := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)
	w.RecordIndexCoverage("main.go", "tree-sitter", "mention", t1, true)
	w.RecordIndexCoverage("main.go", "lsp", "binding", t1, true)
	w.RecordIndexCoverage("util.go", "tree-sitter", "mention", t1, true)
	// A partial-coverage row — LSP gave up on a generated file.
	w.RecordIndexCoverage("generated.pb.go", "lsp", "binding", t1, false)

	require.NoError(t, w.Close())

	cov, err := LoadIndexCoverage(dbPath)
	require.NoError(t, err)
	require.NotNil(t, cov)

	require.Len(t, cov["main.go"], 2, "main.go has both producers")
	require.Len(t, cov["util.go"], 1, "util.go has only tree-sitter")
	require.Len(t, cov["generated.pb.go"], 1, "generated.pb.go has only LSP")

	// Find each entry by producer and verify fields. Order within
	// a source isn't part of the contract (table has no ORDER BY),
	// so look up by producer rather than indexing.
	byProd := func(entries []CoverageEntry, producer string) CoverageEntry {
		for _, e := range entries {
			if e.Producer == producer {
				return e
			}
		}
		t.Fatalf("no entry for producer %q", producer)
		return CoverageEntry{}
	}

	mainTS := byProd(cov["main.go"], "tree-sitter")
	assert.Equal(t, "mention", mainTS.Fidelity)
	assert.True(t, mainTS.Complete)
	assert.True(t, mainTS.IndexedAt.Equal(t1), "IndexedAt must round-trip via UnixNano")

	mainLSP := byProd(cov["main.go"], "lsp")
	assert.Equal(t, "binding", mainLSP.Fidelity)
	assert.True(t, mainLSP.Complete)

	gen := byProd(cov["generated.pb.go"], "lsp")
	assert.False(t, gen.Complete, "complete=false must round-trip — distinguishes partial-index from full-index")
}

func TestSQLiteWriter_RecordIndexCoverage_Replaces(t *testing.T) {
	// PRIMARY KEY (source_id, producer) means a re-index by the same
	// producer overwrites the prior row — neither a constraint
	// violation nor an accumulated history. Different producers keep
	// distinct rows. Pin both halves so a future schema tweak that
	// breaks either can't slip in unnoticed.
	dbPath := filepath.Join(t.TempDir(), "replace.db")
	w, err := NewSQLiteWriter(dbPath)
	require.NoError(t, err)

	t1 := time.Date(2026, 5, 5, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(2 * time.Hour)

	// First indexing: partial.
	w.RecordIndexCoverage("main.go", "tree-sitter", "mention", t1, false)
	// Re-index a couple hours later: now complete.
	w.RecordIndexCoverage("main.go", "tree-sitter", "mention", t2, true)
	// Different producer: separate row, doesn't collide.
	w.RecordIndexCoverage("main.go", "lsp", "binding", t1, true)

	require.NoError(t, w.Close())

	cov, err := LoadIndexCoverage(dbPath)
	require.NoError(t, err)
	require.Len(t, cov["main.go"], 2,
		"replacement on (source_id, producer) — not duplication; LSP keeps its own row")

	// The tree-sitter row reflects the re-index, not the first write.
	for _, e := range cov["main.go"] {
		if e.Producer == "tree-sitter" {
			assert.True(t, e.IndexedAt.Equal(t2), "later indexed_at wins on conflict")
			assert.True(t, e.Complete, "later complete=true wins on conflict")
		}
	}
}

func TestLoadIndexCoverage_NoTableReturnsNilNil(t *testing.T) {
	// A .db without _index_coverage (e.g. one produced before this
	// table existed, or by a partial-write producer that didn't
	// emit coverage rows) returns (nil, nil). The caller treats
	// nil-map as "no coverage info available; assume any absence
	// is unknown" — distinct from the "table present, no rows"
	// case below where a producer ran and observed nothing.
	dbPath := filepath.Join(t.TempDir(), "no_table.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	cov, err := LoadIndexCoverage(dbPath)
	require.NoError(t, err, "missing _index_coverage table is not an error")
	assert.Nil(t, cov, "no table → nil map (caller treats as 'unknown coverage')")
}

func TestLoadIndexCoverage_NonexistentDBReturnsError(t *testing.T) {
	cov, err := LoadIndexCoverage("/nonexistent/path/to/coverage.db")
	require.Error(t, err)
	assert.Nil(t, cov)
}

func TestLoadIndexCoverage_EmptyTableReturnsEmptyMap(t *testing.T) {
	// Distinct from "no table" — table exists, no rows. Returns
	// non-nil empty map so callers can iterate safely.
	dbPath := filepath.Join(t.TempDir(), "empty.db")
	w, err := NewSQLiteWriter(dbPath)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	cov, err := LoadIndexCoverage(dbPath)
	require.NoError(t, err)
	require.NotNil(t, cov)
	assert.Empty(t, cov)
}
