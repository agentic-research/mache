package cmd

import (
	"database/sql"
	"testing"

	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// One-to-many node_hash fan-out guard (B6, mache-ffabd1).
//
// INVARIANT: node_hash is ONE-TO-MANY. A deduped subtree is stored once
// (one node_content row, one node_hash) but appears at MANY occurrences,
// so the same node_hash recurs across many (token, node_id) rows.
// Resolution always targets an occurrence (node_id), NEVER a node_hash;
// a JOIN/GROUP BY on node_hash assuming a single row is a fan-out bug
// (mirror of be6136). These tests pin that consumers surface EVERY
// occurrence of a duplicated subtree — no silent loss.

// b6RealDefHash is a real 32-byte merkle node_hash from the ground-truth
// mergeperfix.db: the def subtree of token `searchResult`, which the
// producer deduped to one node_content row that multiple node_defs
// occurrences point at.
var b6RealDefHash = []byte{
	0xF1, 0xD5, 0xD2, 0x29, 0xEC, 0x89, 0x46, 0x33,
	0xA3, 0x10, 0x92, 0x8B, 0xFC, 0x52, 0xAA, 0xE5,
	0x96, 0xC2, 0x6D, 0xE9, 0xC8, 0x7B, 0x1A, 0xB1,
	0xC9, 0x46, 0x5B, 0x08, 0x9E, 0x2C, 0x4E, 0xE6,
}

// b6RealTopASTHash is the most-duplicated _ast subtree in mergeperfix.db
// (occurs 11,179× across 187 sources) — the strongest available fan-out
// witness.
var b6RealTopASTHash = []byte{
	0x17, 0x7D, 0x27, 0x84, 0x2E, 0x5A, 0xC6, 0x6E,
	0xD6, 0x98, 0x3A, 0x8E, 0x97, 0x40, 0xB3, 0x9B,
	0xF7, 0x32, 0xA7, 0x23, 0x4A, 0xAC, 0x51, 0x4A,
	0x65, 0x71, 0x0D, 0x07, 0x48, 0x8E, 0xEE, 0xE4,
}

// TestEnsureCanonicalViews_NodeHashFanOut (B6 unit) pins the one-to-many
// invariant through the v_defs path: a token whose def subtree is
// deduped to ONE node_hash but occurs at MULTIPLE node_ids must surface
// ALL its occurrences. A consumer that (wrongly) grouped by node_hash
// would collapse them to one — this test would catch that.
func TestEnsureCanonicalViews_NodeHashFanOut(t *testing.T) {
	// Three occurrences of `dup` are ONE deduped subtree living at three
	// distinct node_ids. One unrelated def `solo` is its own subtree.
	//
	// The fixture states the SHARING, not the bytes: node_hash is the
	// producer's content identity, and a test that types a hash literal is
	// asserting on something it does not own.
	b := fixturedb.New(t, fixturedb.Leyline)
	b.Def("dup", "a.go/dup", fixturedb.Function, fixturedb.Detail{Subtree: "dup"})
	b.Def("dup", "b.go/dup", fixturedb.Function, fixturedb.Detail{Subtree: "dup"})
	b.Def("dup", "c.go/dup", fixturedb.Function, fixturedb.Detail{Subtree: "dup"})
	b.Def("solo", "d.go/solo", fixturedb.Function)
	_, f := b.Build()
	db := f.DB()

	dupHash := fixtureSubtreeHash(t, db, "node_defs", "dup")
	soloHash := fixtureSubtreeHash(t, db, "node_defs", "solo")
	require.NotEqual(t, dupHash, soloHash, "unrelated subtrees must not collide")

	// The duplicated node_hash must map to THREE distinct occurrences.
	rows, err := db.Query(
		`SELECT node_id FROM v_defs WHERE token='dup' AND node_hash=? ORDER BY node_id`, dupHash)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		got = append(got, id)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{"a.go/dup", "b.go/dup", "c.go/dup"}, got,
		"one node_hash → many occurrences; v_defs must not collapse the fan-out")

	// Sanity: grouping BY node_hash would wrongly report 1 row for the
	// duplicated subtree — pin that the raw occurrence count is 3.
	var occurrences int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM v_defs WHERE node_hash=?`, dupHash).Scan(&occurrences))
	assert.Equal(t, 3, occurrences, "node_hash is one-to-many, not one-to-one")
}

// TestNodeHashFanOut_GroundTruthDB (B6 integration-flavored) reads the
// real merkle-produced mergeperfix.db and asserts the fan-out survives
// end-to-end: the deduped def subtree of `searchResult` is stored once
// (one node_hash) yet resolves to multiple distinct occurrences, and
// the most-duplicated _ast subtree resolves to many source_id/node_id
// rows — a JOIN on node_hash assuming a single row would be a bug.
//
// Skips when the ground-truth db is absent (CI / other machines).
func TestNodeHashFanOut_GroundTruthDB(t *testing.T) {
	const gtPath = "/private/tmp/b5-verify/mergeperfix.db"
	db, err := sql.Open("sqlite", "file:"+gtPath+"?mode=ro")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	// A missing file surfaces on first query, not on Open — probe and
	// skip rather than fail on machines without the ground-truth db.
	if err := db.QueryRow(`SELECT 1 FROM sqlite_master LIMIT 1`).Scan(new(int)); err != nil {
		t.Skipf("ground-truth db unavailable (%s): %v", gtPath, err)
	}

	// The `searchResult` def subtree deduped to one node_hash but has
	// >1 occurrence. find_definition keys on token → must return ALL.
	var defOccurrences int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM node_defs WHERE token='searchResult' AND node_hash=?`,
		b6RealDefHash,
	).Scan(&defOccurrences))
	assert.Greater(t, defOccurrences, 1,
		"searchResult's deduped def subtree must occur at multiple node_defs rows")

	// _ast: the most-duplicated subtree resolves to MANY (source_id,
	// node_id) rows — one node_hash, many occurrences. Resolving to a
	// single row here would be the be6136-class fan-out bug.
	var astRows, distinctNodes int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*), COUNT(DISTINCT node_id) FROM _ast WHERE node_hash=?`,
		b6RealTopASTHash,
	).Scan(&astRows, &distinctNodes))
	assert.Greater(t, astRows, 1,
		"most-duplicated _ast subtree must resolve to multiple occurrences")
	assert.Equal(t, astRows, distinctNodes,
		"each occurrence is a distinct node_id — node_hash is one-to-many over node_id")
}
