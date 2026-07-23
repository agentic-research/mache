package ingest

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentic-research/mache/internal/graph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestSQLiteWriter_GetNode_RoundTripsProperties pins the bug from bead
// mache-d28eb1: SQLiteWriter.GetNode previously returned only
// ID/Mode/ModTime, dropping Properties. The engine then "preserves"
// Properties across the two-pass write pattern at engine.go:1576-1594,
// but the preservation was a no-op — currentProps was always nil. The
// second pass wrote nil Properties, erasing lang/pkg/imports set on
// the first pass. Net effect: every construct node in `mache build`
// output had only `location`; the MCP find_callees handler (which
// reads `lang` to pick an extractor) returned [] for every construct.
//
// This test writes a dir node with Properties via AddNode, reads it
// back via GetNode, and asserts the Properties round-trip. The bug
// would have been caught immediately if this test had existed.
func TestSQLiteWriter_GetNode_RoundTripsProperties(t *testing.T) {
	dir, err := os.MkdirTemp("", "writer-roundtrip-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	dbPath := filepath.Join(dir, "test.db")
	w, err := NewSQLiteWriter(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })

	w.AddNode(&graph.Node{
		ID:      "pkg/methods/Foo.Bar",
		Mode:    os.ModeDir | 0o555,
		ModTime: time.Unix(1700000000, 0),
		Properties: map[string]json.RawMessage{
			"lang": []byte(`"go"`),
			"pkg":  []byte(`"foo"`),
		},
	})

	got, err := w.GetNode("pkg/methods/Foo.Bar")
	require.NoError(t, err)
	require.True(t, got.Mode.IsDir())
	require.NotNil(t, got.Properties,
		"GetNode must round-trip Properties — the engine's two-pass write "+
			"pattern relies on this to preserve lang/pkg across the location/doc overwrite")
	require.Equal(t, "go", graph.PropString(got, "lang"),
		"lang Property must survive the AddNode → GetNode round-trip")
	require.Equal(t, "foo", graph.PropString(got, "pkg"),
		"pkg Property must survive the round-trip too")
}

// TestSQLiteWriter_RoundTripsContext pins bead mache-b8fe72 on the write
// side: node.Context (the imports/types the headline `cat context` vfile
// serves) must (1) persist into the nodes.context column and (2) survive
// SQLiteWriter.GetNode, which the engine's two-pass write re-reads to
// preserve fields — same class as the Properties fix (mache-d28eb1). If
// GetNode drops Context, the second-pass INSERT OR REPLACE nulls it even
// after the column exists.
func TestSQLiteWriter_RoundTripsContext(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ctx.db")
	ctx := []byte("import (\n\t\"fmt\"\n)\n")

	w, err := NewSQLiteWriter(dbPath)
	require.NoError(t, err)

	w.AddNode(&graph.Node{
		ID:      "pkg/methods/Foo.Bar",
		Mode:    os.ModeDir | 0o555,
		ModTime: time.Unix(1700000000, 0),
		Context: ctx,
	})

	got, err := w.GetNode("pkg/methods/Foo.Bar")
	require.NoError(t, err)
	assert.Equal(t, ctx, got.Context,
		"SQLiteWriter.GetNode must round-trip Context (two-pass write protection)")
	require.NoError(t, w.Close())

	// The bytes actually landed in the nodes.context column.
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	var stored []byte
	require.NoError(t, db.QueryRow(
		`SELECT context FROM nodes WHERE id = ?`, "pkg/methods/Foo.Bar").Scan(&stored))
	assert.Equal(t, ctx, stored, "AddNode must persist node.Context to the context column")
}

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

// v_defs / v_refs canonical views — ADR-0013 Step 3 (mache-346d0f).
// The contract: consumers query these views instead of node_defs /
// node_refs directly. Today the views surface mention-fidelity rows
// only; Step 1 (sister bead ley-line-453f7e) extends the view body
// with binding-fidelity rows from _lsp_*. Consumer SQL stays
// stable across that expansion.
//
// Tests pin the view shape (columns, fidelity tag) and the
// idempotence of EnsureCanonicalViews so Step 4 can adopt it on
// .dbs that don't already have the views.

// queryRows is a small helper that scans (token, node_id, fidelity)
// triples from any view shaped like v_defs.
func queryRows(t *testing.T, db *sql.DB, q string) []defRow {
	t.Helper()
	rows, err := db.Query(q)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	var out []defRow
	for rows.Next() {
		var r defRow
		require.NoError(t, rows.Scan(&r.token, &r.nodeID, &r.fidelity))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

type defRow struct{ token, nodeID, fidelity string }

func TestCanonicalViews_MentionFidelityRoundTrip(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "views.db")
	w, err := NewSQLiteWriter(dbPath)
	require.NoError(t, err)

	// Tree-sitter rows go straight into node_defs / node_refs via
	// the writer's own statements. Bypass AddNode and write a few
	// rows directly to keep the test focused on the view layer.
	_, err = w.tx.Exec("INSERT INTO node_defs (token, node_id) VALUES (?, ?), (?, ?)",
		"Validate", "auth/functions/Validate",
		"Charge", "billing/functions/Charge")
	require.NoError(t, err)
	_, err = w.tx.Exec("INSERT INTO node_refs (token, node_id) VALUES (?, ?)",
		"Validate", "billing/functions/Charge")
	require.NoError(t, err)
	require.NoError(t, w.Close())

	// Reopen read-only and query the views.
	db, err := sql.Open("sqlite", dbPath+"?mode=ro")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	defs := queryRows(t, db, "SELECT token, node_id, fidelity FROM v_defs ORDER BY token")
	require.Len(t, defs, 2)
	assert.Equal(t, "Charge", defs[0].token)
	assert.Equal(t, "Validate", defs[1].token)
	for _, d := range defs {
		assert.Equal(t, "mention", d.fidelity,
			"v_defs surfaces only mention-fidelity rows pre-Step-1; binding rows arrive once ley-line-453f7e ships")
	}

	// v_refs has more columns; just confirm the tagged-fidelity
	// row is there and the LSP-only columns are NULL until Step 1.
	type refRow struct {
		referrer, token, fidelity        string
		targetNodeID, refURI, refLineRaw sql.NullString
	}
	rows, err := db.Query(`SELECT referrer_node_id, token, target_node_id, ref_uri, ref_line, fidelity FROM v_refs`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	var got []refRow
	for rows.Next() {
		var r refRow
		require.NoError(t, rows.Scan(&r.referrer, &r.token, &r.targetNodeID, &r.refURI, &r.refLineRaw, &r.fidelity))
		got = append(got, r)
	}
	require.NoError(t, rows.Err())
	require.Len(t, got, 1)
	assert.Equal(t, "billing/functions/Charge", got[0].referrer)
	assert.Equal(t, "Validate", got[0].token)
	assert.Equal(t, "mention", got[0].fidelity)
	assert.False(t, got[0].targetNodeID.Valid, "binding columns are NULL at L_0")
	assert.False(t, got[0].refURI.Valid, "binding columns are NULL at L_0")
}

func TestCanonicalViews_EmptyTablesYieldEmptyViews(t *testing.T) {
	// Fresh writer with no rows inserted. Views resolve, return
	// zero rows, no errors. Callers (Step 4 rule rewrites) need to
	// handle the empty case without special-casing.
	dbPath := filepath.Join(t.TempDir(), "empty_views.db")
	w, err := NewSQLiteWriter(dbPath)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	db, err := sql.Open("sqlite", dbPath+"?mode=ro")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	var defCount, refCount int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM v_defs").Scan(&defCount))
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM v_refs").Scan(&refCount))
	assert.Equal(t, 0, defCount)
	assert.Equal(t, 0, refCount)
}

func TestEnsureCanonicalViews_Idempotent(t *testing.T) {
	// EnsureCanonicalViews installs the views on a .db that doesn't
	// have them yet — e.g. an LLO build pre-migration. Running it
	// twice in a row is safe (CREATE VIEW IF NOT EXISTS).
	dbPath := filepath.Join(t.TempDir(), "ensure.db")

	// Hand-build a .db with node_defs / node_refs but NO views. This
	// simulates a producer that wrote the underlying tables without
	// running mache's NewSQLiteWriter — exactly the LLO situation.
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	_, err = db.Exec(`
		CREATE TABLE node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id));
		CREATE TABLE node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id));
		INSERT INTO node_defs (token, node_id) VALUES ('Foo', 'pkg/Foo');
	`)
	require.NoError(t, err)

	// Confirm view doesn't exist yet.
	var name string
	err = db.QueryRow("SELECT name FROM sqlite_master WHERE type='view' AND name='v_defs'").Scan(&name)
	assert.ErrorIs(t, err, sql.ErrNoRows, "v_defs should not exist on a hand-built .db")

	// Install. Then install again — must be idempotent.
	require.NoError(t, EnsureCanonicalViews(db))
	require.NoError(t, EnsureCanonicalViews(db))

	// View now resolves and surfaces the underlying row.
	defs := queryRows(t, db, "SELECT token, node_id, fidelity FROM v_defs")
	require.Len(t, defs, 1)
	assert.Equal(t, "Foo", defs[0].token)
	assert.Equal(t, "mention", defs[0].fidelity)

	require.NoError(t, db.Close())
}
