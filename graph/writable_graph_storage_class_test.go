package graph

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// Regression for mache-6bed54.
//
// nodes.record is written by two paths that used to bind different Go types,
// which SQLite stores as different storage classes:
//
//	ingest      SQLiteWriter bound n.Data ([]byte)      -> BLOB
//	write-back  WritableGraph.UpdateRecord binds string -> TEXT
//
// SQLite never considers a BLOB equal to a TEXT value, so `WHERE record = ?`
// matched only nodes written back through the mount and silently missed every
// ingested node — nearly all of them. LIKE and length() coerce across storage
// classes and kept working, which is why nothing caught it.
//
// The invariant is that both writers agree. This writes the same content both
// ways and requires storage class AND equality behaviour to be
// indistinguishable.
func TestRecordStorageClass_BothWritersAgree(t *testing.T) {
	const content = "func f() int { return 7 }"

	dbPath := filepath.Join(t.TempDir(), "storage.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE nodes (
		id TEXT PRIMARY KEY,
		parent_id TEXT,
		name TEXT NOT NULL,
		kind INTEGER NOT NULL,
		size INTEGER DEFAULT 0,
		mtime INTEGER NOT NULL,
		record_id TEXT,
		record TEXT,
		source_file TEXT,
		context BLOB,
		props JSON
	)`)
	require.NoError(t, err)

	// Path 1 — the ingest bind. SQLiteWriter builds a *string and binds that;
	// mirrored here rather than imported, since internal/ingest depends on this
	// package. The production bind is pinned separately by
	// internal/ingest/sqlite_writer_affinity_test.go.
	s := content
	_, err = db.Exec(
		`INSERT INTO nodes (id, name, kind, size, mtime, record)
		 VALUES ('ingested','ingested',0,?,0,?)`, len(content), &s)
	require.NoError(t, err)

	// Path 2 — the real write-back.
	_, err = db.Exec(
		`INSERT INTO nodes (id, name, kind, size, mtime, record)
		 VALUES ('writtenback','writtenback',0,0,0,NULL)`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	g, err := OpenWritableGraph(dbPath, &api.Topology{Table: "results"}, stubRender, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })
	require.NoError(t, g.UpdateRecord("writtenback", []byte(content)))

	verify, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = verify.Close() }()

	var ingestedType, writtenType string
	require.NoError(t, verify.QueryRow(
		"SELECT typeof(record) FROM nodes WHERE id='ingested'").Scan(&ingestedType))
	require.NoError(t, verify.QueryRow(
		"SELECT typeof(record) FROM nodes WHERE id='writtenback'").Scan(&writtenType))

	assert.Equal(t, ingestedType, writtenType,
		"both writers must produce the same storage class; a mismatch makes "+
			"`WHERE record = ?` depend on which writer last touched the node")
	assert.Equal(t, "text", ingestedType,
		"record holds text content in a TEXT-affinity column, so TEXT is the "+
			"storage class both writers should produce")

	// The observable consequence, asserted directly rather than inferred from
	// typeof: exact-match search must find both nodes or neither.
	var matched int
	require.NoError(t, verify.QueryRow(
		"SELECT count(*) FROM nodes WHERE record = ?", content).Scan(&matched))
	assert.Equal(t, 2, matched,
		"exact-match search must not depend on which writer wrote the node")

	// LIKE always coerced, so it must keep working — guards against a "fix"
	// that trades one broken operator for another.
	require.NoError(t, verify.QueryRow(
		"SELECT count(*) FROM nodes WHERE record LIKE '%return 7%'").Scan(&matched))
	assert.Equal(t, 2, matched, "LIKE worked before and must still work")
}
