package cmd

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// Additive node_hash passthrough (B4, mache-ff9a9d).
//
// ensureCanonicalViews surfaces node_hash as a trailing additive column
// on v_defs / v_refs when the occurrence tables (node_defs / node_refs)
// carry it — the merkle-AST producer dedups identical subtrees to one
// node_content row keyed by a 32-byte node_hash and stamps a pointer
// onto each occurrence. When the column is absent (standalone-mache db)
// the views emit `NULL AS node_hash`, keeping a stable trailing-column
// shape and behaving exactly as today. The token/node_id keying is
// untouched.

// TestEnsureCanonicalViews_NodeHashPassthrough (B4a) pins that when
// node_defs / node_refs carry an additive node_hash column, v_defs /
// v_refs expose it, and the existing token/node_id/fidelity columns
// stay byte-identical.
func TestEnsureCanonicalViews_NodeHashPassthrough(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "nodehash.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// Mirror the merkle-producer schema: node_defs / node_refs with the
	// additive node_hash BLOB column referencing node_content.
	_, err = db.Exec(`
		CREATE TABLE node_defs (
			token TEXT NOT NULL, node_id TEXT NOT NULL,
			source_id TEXT NOT NULL, node_hash BLOB
		);
		CREATE TABLE node_refs (
			token TEXT NOT NULL, node_id TEXT NOT NULL,
			source_id TEXT NOT NULL, node_hash BLOB
		);
		INSERT INTO node_defs VALUES ('Foo', 'pkg/Foo', 'pkg.go', X'AABB');
		INSERT INTO node_refs VALUES ('Foo', 'pkg/Bar', 'pkg.go', X'CCDD');
	`)
	require.NoError(t, err)

	qg := &sqlDBQuerier{db: db}
	require.NoError(t, ensureCanonicalViews(qg))

	// v_defs: existing columns unchanged, node_hash exposed.
	var (
		token, nodeID, fidelity string
		nodeHash                []byte
	)
	require.NoError(t, db.QueryRow(
		`SELECT token, node_id, fidelity, node_hash FROM v_defs WHERE fidelity='mention'`,
	).Scan(&token, &nodeID, &fidelity, &nodeHash))
	assert.Equal(t, "Foo", token)
	assert.Equal(t, "pkg/Foo", nodeID)
	assert.Equal(t, "mention", fidelity)
	assert.Equal(t, []byte{0xAA, 0xBB}, nodeHash, "v_defs must passthrough node_defs.node_hash")

	// v_refs: existing columns unchanged, node_hash exposed.
	var (
		refReferrer, refToken, refQualifier string
		refHash                             []byte
	)
	require.NoError(t, db.QueryRow(
		`SELECT referrer_node_id, token, qualifier, node_hash FROM v_refs WHERE fidelity='mention'`,
	).Scan(&refReferrer, &refToken, &refQualifier, &refHash))
	assert.Equal(t, "pkg/Bar", refReferrer)
	assert.Equal(t, "Foo", refToken)
	assert.Equal(t, "", refQualifier)
	assert.Equal(t, []byte{0xCC, 0xDD}, refHash, "v_refs must passthrough node_refs.node_hash")
}

// TestEnsureCanonicalViews_NodeHashAbsentBackCompat (B4b) is the
// backward-compat regression: a standalone-mache db has no node_hash
// column. The views must still build, the token/node_id rows must be
// identical to today, and node_hash must read as NULL (stable trailing
// column shape) rather than erroring with "no such column".
func TestEnsureCanonicalViews_NodeHashAbsentBackCompat(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "no_nodehash.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// Legacy standalone shape: no node_hash column at all.
	_, err = db.Exec(`
		CREATE TABLE node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		INSERT INTO node_defs VALUES ('Foo', 'pkg/Foo');
		INSERT INTO node_refs VALUES ('Foo', 'pkg/Bar');
	`)
	require.NoError(t, err)

	qg := &sqlDBQuerier{db: db}
	require.NoError(t, ensureCanonicalViews(qg))

	// Existing rows byte-identical to today.
	var token, nodeID string
	require.NoError(t, db.QueryRow(
		`SELECT token, node_id FROM v_defs WHERE fidelity='mention'`,
	).Scan(&token, &nodeID))
	assert.Equal(t, "Foo", token)
	assert.Equal(t, "pkg/Foo", nodeID)

	// node_hash column present in shape, resolves to NULL (not an error).
	var defHash, refHash []byte
	require.NoError(t, db.QueryRow(`SELECT node_hash FROM v_defs WHERE fidelity='mention'`).Scan(&defHash))
	require.NoError(t, db.QueryRow(`SELECT node_hash FROM v_refs WHERE fidelity='mention'`).Scan(&refHash))
	assert.Nil(t, defHash, "no node_defs.node_hash → NULL, not an error")
	assert.Nil(t, refHash, "no node_refs.node_hash → NULL, not an error")

	// Row counts identical to today's mention-only behavior.
	var defCount, refCount int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM v_defs`).Scan(&defCount))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM v_refs`).Scan(&refCount))
	assert.Equal(t, 1, defCount)
	assert.Equal(t, 1, refCount)
}
