package graph

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// One-to-many node_hash fan-out guard on the reader path (B6,
// mache-ffabd1).
//
// The merkle-AST producer dedups identical subtrees to one node_content
// row keyed by a 32-byte node_hash and stamps an additive node_hash
// pointer onto each OCCURRENCE in node_defs / node_refs. mache reads the
// occurrence layer keyed on token / node_id — node_hash is never a key.
//
// INVARIANT: node_hash is ONE-TO-MANY. A token whose def (or ref)
// subtree is duplicated has the SAME node_hash across MANY distinct
// node_ids. find_definition / find_callers must return ALL of them —
// keying/deduping on node_hash would silently drop occurrences (the
// be6136-class fan-out bug). These tests seed a duplicated node_hash and
// assert no occurrence is lost. The additive column must also not
// disturb the existing token/node_id read path.

// TestSQLiteGraph_LookupDef_NodeHashFanOut pins that LookupDef returns
// every occurrence of a token whose def subtree deduped to a single
// node_hash. The reader keys on token; the additive node_hash column is
// along for the ride and must not collapse the result set.
func TestSQLiteGraph_LookupDef_NodeHashFanOut(t *testing.T) {
	dir, err := os.MkdirTemp("", "lookupdef-nodehash-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	dbPath := filepath.Join(dir, "test.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	// node_defs / node_refs carry the additive node_hash BLOB column,
	// as the merkle producer writes it.
	_, err = db.Exec(`
		CREATE TABLE nodes (
			id TEXT PRIMARY KEY, parent_id TEXT, name TEXT NOT NULL,
			kind INTEGER NOT NULL, size INTEGER DEFAULT 0, mtime INTEGER NOT NULL,
			record_id TEXT, record JSON
		);
		CREATE TABLE node_refs (
			token TEXT NOT NULL, node_id TEXT NOT NULL, node_hash BLOB
		);
		CREATE TABLE node_defs (
			token TEXT NOT NULL, node_id TEXT NOT NULL, node_hash BLOB
		);

		-- Three occurrences of searchResult share ONE deduped node_hash
		-- (X'DEAD') at three distinct node_ids. A JOIN/GROUP BY on
		-- node_hash would collapse these to one.
		INSERT INTO node_defs VALUES ('searchResult', 'a/searchResult', X'DEAD');
		INSERT INTO node_defs VALUES ('searchResult', 'b/searchResult', X'DEAD');
		INSERT INTO node_defs VALUES ('searchResult', 'c/searchResult', X'DEAD');
	`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	g, err := OpenSQLiteGraph(dbPath, &api.Topology{}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })

	ids := g.LookupDef("searchResult")
	require.ElementsMatch(t,
		[]string{"a/searchResult", "b/searchResult", "c/searchResult"}, ids,
		"LookupDef must return ALL occurrences of a token whose def subtree "+
			"deduped to one node_hash — no silent loss to the fan-out")

	// DefsMap (bulk snapshot path) must preserve the same fan-out.
	got := g.DefsMap()
	require.Len(t, got["searchResult"], 3,
		"DefsMap must hydrate all node_hash-duplicated occurrences")
}

// TestSQLiteGraph_GetCallers_NodeHashFanOut pins the same invariant on
// the refs/find_callers path: a duplicated ref subtree (one node_hash,
// many referrers) must surface every caller.
func TestSQLiteGraph_GetCallers_NodeHashFanOut(t *testing.T) {
	dir, err := os.MkdirTemp("", "getcallers-nodehash-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	dbPath := filepath.Join(dir, "test.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	_, err = db.Exec(`
		CREATE TABLE nodes (
			id TEXT PRIMARY KEY, parent_id TEXT, name TEXT NOT NULL,
			kind INTEGER NOT NULL, size INTEGER DEFAULT 0, mtime INTEGER NOT NULL,
			record_id TEXT, record JSON
		);
		CREATE TABLE node_refs (
			token TEXT NOT NULL, node_id TEXT NOT NULL, node_hash BLOB
		);
		CREATE TABLE node_defs (
			token TEXT NOT NULL, node_id TEXT NOT NULL, node_hash BLOB
		);

		-- Two call sites reference Validate from identical (deduped)
		-- ref subtrees — same node_hash, distinct referrer node_ids.
		INSERT INTO node_refs VALUES ('Validate', 'billing/Charge', X'CAFE');
		INSERT INTO node_refs VALUES ('Validate', 'auth/Login',    X'CAFE');
	`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	g, err := OpenSQLiteGraph(dbPath, &api.Topology{}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })

	callers, err := g.GetCallers("Validate")
	require.NoError(t, err)
	ids := make([]string, len(callers))
	for i, n := range callers {
		ids[i] = n.ID
	}
	require.ElementsMatch(t, []string{"billing/Charge", "auth/Login"}, ids,
		"GetCallers must return every referrer even when their ref subtrees "+
			"deduped to a single node_hash")
}
