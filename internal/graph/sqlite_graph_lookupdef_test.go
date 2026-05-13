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

// TestSQLiteGraph_LookupDef_SQLFallback regression-guards the bug
// surfaced by the PR #373 benchmark: find_definition returned "not
// found" for every test symbol on a pre-built .db, including unique
// symbols like dedupSuffix. Root cause was that LookupDef only
// consulted the in-memory g.defs map, while the actual defs lived in
// the node_defs SQL table (populated by leyline parse / mache build).
//
// This test sets up a minimal nodes-table-shaped DB with one
// node_defs row and asserts LookupDef returns it without any prior
// AddDef call — exercising the SQL-fallback path added alongside
// this test.
func TestSQLiteGraph_LookupDef_SQLFallback(t *testing.T) {
	dir, err := os.MkdirTemp("", "lookupdef-sqlfallback-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	dbPath := filepath.Join(dir, "test.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	_, err = db.Exec(`
		CREATE TABLE nodes (
			id TEXT PRIMARY KEY,
			parent_id TEXT,
			name TEXT NOT NULL,
			kind INTEGER NOT NULL,
			size INTEGER DEFAULT 0,
			mtime INTEGER NOT NULL,
			record_id TEXT,
			record JSON
		);
		CREATE INDEX idx_parent_name ON nodes(parent_id, name);

		CREATE TABLE node_refs (
			token TEXT,
			node_id TEXT,
			PRIMARY KEY (token, node_id)
		) WITHOUT ROWID;

		CREATE TABLE node_defs (
			token TEXT,
			node_id TEXT,
			PRIMARY KEY (token, node_id)
		) WITHOUT ROWID;

		INSERT INTO nodes VALUES ('functions', '', 'functions', 1, 0, 1000, NULL, NULL);
		INSERT INTO nodes VALUES ('functions/dedupSuffix', 'functions', 'dedupSuffix', 1, 0, 2000, NULL, NULL);

		INSERT INTO node_defs VALUES ('dedupSuffix', 'functions/dedupSuffix');
		INSERT INTO node_defs VALUES ('Engine.dedupSuffix', 'functions/dedupSuffix');
	`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	g, err := OpenSQLiteGraph(dbPath, &api.Topology{}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })

	// Bare token: must resolve via node_defs even though no AddDef
	// was ever called in this process. This is the bug from PR #373.
	ids := g.LookupDef("dedupSuffix")
	require.Equal(t, []string{"functions/dedupSuffix"}, ids,
		"LookupDef must fall back to node_defs SQL when in-memory g.defs is empty")

	// Qualified token: same path, just a different key.
	qualIDs := g.LookupDef("Engine.dedupSuffix")
	require.Equal(t, []string{"functions/dedupSuffix"}, qualIDs,
		"LookupDef SQL fallback must handle qualified tokens identically")

	// Unknown token: still returns nil, not a phantom match.
	unknown := g.LookupDef("NotInTheTable")
	require.Nil(t, unknown,
		"LookupDef must return nil for tokens absent from node_defs (no phantom hits)")
}

// TestSQLiteGraph_LookupDef_InMemoryWinsOverSQL pins that an AddDef
// call still takes precedence over the SQL table — the in-memory map
// is a write-through layer for live ingestion, not just a cache. If
// a caller has added an unflushed def via AddDef, LookupDef should
// see it without dropping back to SQL.
func TestSQLiteGraph_LookupDef_InMemoryWinsOverSQL(t *testing.T) {
	dir, err := os.MkdirTemp("", "lookupdef-inmem-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	dbPath := filepath.Join(dir, "test.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	_, err = db.Exec(`
		CREATE TABLE nodes (
			id TEXT PRIMARY KEY,
			parent_id TEXT,
			name TEXT NOT NULL,
			kind INTEGER NOT NULL,
			size INTEGER DEFAULT 0,
			mtime INTEGER NOT NULL,
			record_id TEXT,
			record JSON
		);
		CREATE TABLE node_refs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		CREATE TABLE node_defs (token TEXT, node_id TEXT, PRIMARY KEY (token, node_id)) WITHOUT ROWID;
		INSERT INTO node_defs VALUES ('Foo', 'sql/Foo');
	`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	g, err := OpenSQLiteGraph(dbPath, &api.Topology{}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })

	// Pre-populate in-memory with a different node_id for "Foo".
	require.NoError(t, g.AddDef("Foo", "memory/Foo"))

	got := g.LookupDef("Foo")
	require.Equal(t, []string{"memory/Foo"}, got,
		"in-memory AddDef must take precedence — no double-lookup that returns SQL+memory entries")
}
