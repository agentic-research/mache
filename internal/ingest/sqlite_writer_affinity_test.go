package ingest

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"
)

// Regression for mache-4b8a42 — nodes.record must not have NUMERIC affinity.
//
// SQLite has no JSON storage class. Affinity is chosen by substring match on
// the DECLARED type, and the literal "JSON" contains none of the substrings
// that select TEXT, INTEGER, REAL, or BLOB affinity — so a column declared
// `record JSON` falls through to NUMERIC, which silently converts any TEXT
// value that parses as a well-formed number: '007' is stored and read back
// as 7. Declaring it TEXT is the fix.
//
// Reachability is asymmetric across the two writers, which is why the
// declaration itself is the invariant worth pinning rather than any one path:
//
//	INGEST  — SQLiteWriter binds n.Data ([]byte), which SQLite stores as a BLOB.
//	          NUMERIC affinity does not convert BLOBs, so the ingest path is
//	          immune today. It is immune by accident of the bind type, not by
//	          design: anything that starts binding a string here inherits the
//	          bug silently.
//
//	WRITEBACK — WritableGraph.UpdateRecord (graph/writable_graph.go:99)
//	          binds string(content). That is TEXT, and TEXT is exactly what
//	          NUMERIC affinity rewrites. Editing a projected file through the
//	          mount so its whole body is '007' stores 7 — and since `size` is
//	          bound separately as len(content), size and content desync too.
//
// So the fix is the declared type, and this test asserts it directly.

// affinityCases are values a file body can legitimately hold that SQLite's
// NUMERIC affinity would rewrite, plus controls that must survive regardless.
var affinityCases = []struct {
	name    string
	content string
	why     string
}{
	{"zero padded id", "007", "leading zeros lost: 007 -> 7"},
	{"trailing zero version", "1.10", "1.10 != 1.1 for exact match or version compare"},
	{"exponent notation", "1e3", "notation rewritten: 1e3 -> 1000"},
	{"decimal zero", "0.0", "0.0 -> 0"},
	{"leading zero decimal", "0.50", "0.50 -> 0.5"},
	{"signed", "+1", "sign dropped: +1 -> 1"},
	{"hex like", "0x1F", "control: not a SQLite numeric literal"},
	{"infinity word", "Infinity", "control: plain text"},
	{"source body", "func f() int { return 7 }", "control: real source"},
	{"padded number", " 42 ", "SQLite trims whitespace when converting: ' 42 ' -> 42"},
}

// TestRecordColumn_HasTextAffinity pins the root cause. Declared type is what
// determines affinity, so this is the invariant; the round-trip below is its
// observable consequence.
func TestRecordColumn_HasTextAffinity(t *testing.T) {
	db := writerCreatedDB(t)

	rows, err := db.Query("SELECT name, type FROM pragma_table_info('nodes')")
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	declared := map[string]string{}
	for rows.Next() {
		var n, ty string
		require.NoError(t, rows.Scan(&n, &ty))
		declared[n] = ty
	}
	require.NoError(t, rows.Err())

	assert.Equal(t, "TEXT", declared["record"],
		"nodes.record holds rendered content and must have TEXT affinity; a declared "+
			"type of JSON matches no affinity substring and falls through to NUMERIC, "+
			"silently converting '007' to 7 (mache-4b8a42)")
}

// TestRecordColumn_TextWriteRoundTripsExactly exercises the reachable path:
// a TEXT bind, exactly as WritableGraph.UpdateRecord performs on every
// write-back through the mount.
func TestRecordColumn_TextWriteRoundTripsExactly(t *testing.T) {
	db := writerCreatedDB(t)

	for _, tc := range affinityCases {
		t.Run(tc.name, func(t *testing.T) {
			id := "n/" + tc.name
			_, err := db.Exec(
				`INSERT INTO nodes (id, name, kind, size, mtime, record) VALUES (?, ?, 0, ?, 0, ?)`,
				id, tc.name, len(tc.content), tc.content, // string bind — mirrors UpdateRecord
			)
			require.NoError(t, err)

			var got string
			var typ string
			var size int
			require.NoError(t, db.QueryRow(
				"SELECT record, typeof(record), size FROM nodes WHERE id = ?", id,
			).Scan(&got, &typ, &size))

			assert.Equal(t, tc.content, got, "content must round-trip byte-for-byte — %s", tc.why)
			assert.Equal(t, "text", typ, "a TEXT bind must stay TEXT, got %q", typ)
			assert.Equal(t, len(got), size,
				"size is bound from len(content); a converted record desyncs it")
		})
	}
}

// TestRecordColumn_BlobWriteIsUnaffected documents why ingest never tripped
// this. If SQLiteWriter ever switches to binding a string, the test above is
// what catches it — this one just records that BLOB storage was the reason the
// bug stayed latent, so nobody concludes the JSON declaration was harmless.
func TestRecordColumn_BlobWriteIsUnaffected(t *testing.T) {
	db := writerCreatedDB(t)
	_, err := db.Exec(
		`INSERT INTO nodes (id, name, kind, size, mtime, record) VALUES ('b','b',0,3,0,?)`,
		[]byte("007"),
	)
	require.NoError(t, err)

	var got []byte
	var typ string
	require.NoError(t, db.QueryRow("SELECT record, typeof(record) FROM nodes WHERE id='b'").
		Scan(&got, &typ))
	assert.Equal(t, "007", string(got))
	assert.Equal(t, "blob", typ, "BLOB binds bypass affinity conversion entirely")
}

// writerCreatedDB returns a connection to a database whose schema was created
// by the production SQLiteWriter, so these tests track the real DDL rather than
// a copy that can drift.
func writerCreatedDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "affinity.db")
	w, err := NewSQLiteWriter(path)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}
