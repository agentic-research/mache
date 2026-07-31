package cmd

import (
	"database/sql"
	"testing"

	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	// The merkle producer IS ley-line: node_hash is its additive column, so
	// this arm is only reachable on that producer. Saying so names the arm
	// instead of implying it through a hand-written column list.
	b := fixturedb.New(t, fixturedb.Leyline)
	b.Def("Foo", "pkg.go/functions/Foo", fixturedb.Function, fixturedb.Detail{Subtree: "foo-def"})
	b.Ref("Foo", "pkg.go/functions/Bar", "", "", fixturedb.Detail{Subtree: "foo-ref"})
	_, f := b.Build()
	db := f.DB()

	defHashWant := fixtureSubtreeHash(t, db, "node_defs", "Foo")
	refHashWant := fixtureSubtreeHash(t, db, "node_refs", "Foo")
	require.NotEqual(t, defHashWant, refHashWant, "distinct subtrees must not collide")

	// v_defs: existing columns unchanged, node_hash exposed.
	var (
		token, nodeID, fidelity string
		nodeHash                []byte
	)
	require.NoError(t, db.QueryRow(
		`SELECT token, node_id, fidelity, node_hash FROM v_defs WHERE fidelity='mention'`,
	).Scan(&token, &nodeID, &fidelity, &nodeHash))
	assert.Equal(t, "Foo", token)
	assert.Equal(t, "pkg.go/functions/Foo", nodeID)
	assert.Equal(t, "mention", fidelity)
	assert.Equal(t, defHashWant, nodeHash, "v_defs must passthrough node_defs.node_hash")

	// v_refs: existing columns unchanged, node_hash exposed.
	var (
		refReferrer, refToken, refQualifier string
		refHash                             []byte
	)
	require.NoError(t, db.QueryRow(
		`SELECT referrer_node_id, token, qualifier, node_hash FROM v_refs WHERE fidelity='mention'`,
	).Scan(&refReferrer, &refToken, &refQualifier, &refHash))
	assert.Equal(t, "pkg.go/functions/Bar", refReferrer,
		"the referrer is the ENCLOSING construct — on ley-line that is container_node_id, "+
			"not the call-site leaf in node_id")
	assert.Equal(t, "Foo", refToken)
	assert.Equal(t, "", refQualifier)
	assert.Equal(t, refHashWant, refHash, "v_refs must passthrough node_refs.node_hash")
}

// TestEnsureCanonicalViews_NodeHashAbsentBackCompat (B4b) is the
// backward-compat regression: a standalone-mache db has no node_hash
// column. The views must still build, the token/node_id rows must be
// identical to today, and node_hash must read as NULL (stable trailing
// column shape) rather than erroring with "no such column".
func TestEnsureCanonicalViews_NodeHashAbsentBackCompat(t *testing.T) {
	// "A standalone-mache db has no node_hash column" is not a legacy quirk to
	// be re-typed per fixture — it is what fixturedb.Standalone IS.
	b := fixturedb.New(t, fixturedb.Standalone)
	b.Def("Foo", "pkg/functions/Foo", fixturedb.Function)
	b.Ref("Foo", "pkg/functions/Bar", "", "")
	_, f := b.Build()
	db := f.DB()

	// Existing rows byte-identical to today.
	var token, nodeID string
	require.NoError(t, db.QueryRow(
		`SELECT token, node_id FROM v_defs WHERE fidelity='mention'`,
	).Scan(&token, &nodeID))
	assert.Equal(t, "Foo", token)
	assert.Equal(t, "pkg/functions/Foo", nodeID)

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

// fixtureSubtreeHash reads back the node_hash a fixture assigned to a token's
// occurrence. Tests assert against THIS rather than a literal, because a
// fixture states which occurrences share a subtree — never the bytes that
// identify one.
func fixtureSubtreeHash(t *testing.T, db *sql.DB, table, token string) []byte {
	t.Helper()
	var h []byte
	switch table {
	case "node_defs":
		require.NoError(t, db.QueryRow(
			`SELECT node_hash FROM node_defs WHERE token = ?`, token).Scan(&h))
	default:
		require.NoError(t, db.QueryRow(
			`SELECT node_hash FROM node_refs WHERE token = ?`, token).Scan(&h))
	}
	require.NotEmpty(t, h, "the fixture must have stamped a node_hash")
	return h
}
