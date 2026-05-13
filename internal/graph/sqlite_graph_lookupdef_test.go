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

// TestSQLiteGraph_SearchDefs_SQLFallback pins the SQL-pushdown path
// for `search role=definition`. Before this fix the handler called
// DefsMap() — empty on pre-built .db files — and returned [] for
// every pattern. Bead mache-9cba08.
func TestSQLiteGraph_SearchDefs_SQLFallback(t *testing.T) {
	dir, err := os.MkdirTemp("", "searchdefs-sqlfallback-*")
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

		INSERT INTO node_defs VALUES ('Topology', 'api/types/Topology');
		INSERT INTO node_defs VALUES ('NewMemoryStore', 'graph/functions/NewMemoryStore');
		INSERT INTO node_defs VALUES ('MemoryStore', 'graph/types/MemoryStore');
		INSERT INTO node_defs VALUES ('NewSomethingElse', 'other/functions/NewSomethingElse');
	`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	g, err := OpenSQLiteGraph(dbPath, &api.Topology{}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })

	// SQL LIKE 'New%' should match the two New-prefixed entries.
	matches := g.SearchDefs("New%", 100)
	require.Len(t, matches, 2, "SearchDefs must hit the two New-prefixed defs via SQL LIKE pushdown")
	require.Contains(t, matches, "NewMemoryStore")
	require.Contains(t, matches, "NewSomethingElse")

	// Exact-match pattern should return one entry.
	exact := g.SearchDefs("Topology", 100)
	require.Equal(t, map[string][]string{"Topology": {"api/types/Topology"}}, exact)

	// Limit must be honored.
	limited := g.SearchDefs("%", 2)
	require.LessOrEqual(t, len(limited), 2, "SearchDefs must honor the limit parameter")

	// Unknown pattern returns empty (not nil).
	none := g.SearchDefs("DoesNotExist", 100)
	require.Empty(t, none, "no match must return empty map, not phantom hits")
}

// TestSQLiteGraph_GetCallees_ReceiverSuffixMatch pins the resolver
// fallback for receiver-method calls (bead mache-9ca6af). A call
// like `c.sendRaw(...)` extracts as bare "sendRaw" in node_refs;
// node_defs has it only as the qualified "SocketClient.sendRaw".
// Without the suffix fallback, find_callees returned [] for SendOp
// despite the visible c.sendRaw call in its body.
func TestSQLiteGraph_GetCallees_ReceiverSuffixMatch(t *testing.T) {
	dir, err := os.MkdirTemp("", "callees-suffix-*")
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

		-- A construct SendOp whose body calls c.sendRaw
		INSERT INTO nodes VALUES ('leyline/methods/SendOp', 'leyline/methods', 'SendOp', 1, 0, 1, NULL, NULL);
		INSERT INTO nodes VALUES ('leyline/methods/SendOp/source', 'leyline/methods/SendOp', 'source', 0, 100, 1, NULL, 'func (c *SocketClient) SendOp() { c.sendRaw() }');
		-- The sendRaw definition lives at the qualified form
		INSERT INTO nodes VALUES ('leyline/methods/SocketClient.sendRaw', 'leyline/methods', 'SocketClient.sendRaw', 1, 0, 1, NULL, NULL);
		INSERT INTO nodes VALUES ('leyline/methods/SocketClient.sendRaw/source', 'leyline/methods/SocketClient.sendRaw', 'source', 0, 30, 1, NULL, 'func (c *SocketClient) sendRaw() {}');

		-- Extraction of c.sendRaw lands as bare token "sendRaw"
		INSERT INTO node_refs VALUES ('sendRaw', 'leyline/methods/SendOp/source');

		-- Defs only carry the qualified form (production reality)
		INSERT INTO node_defs VALUES ('SocketClient.sendRaw', 'leyline/methods/SocketClient.sendRaw');
		INSERT INTO node_defs VALUES ('SocketClient.SendOp', 'leyline/methods/SendOp');
	`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	g, err := OpenSQLiteGraph(dbPath, &api.Topology{}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })

	// Set the call extractor that the GetCallees path needs.
	g.SetCallExtractor(func(_ []byte, _, _ string) ([]QualifiedCall, error) {
		// Mimics what ExtractQualifiedCalls would produce for `c.sendRaw()`:
		// bare token, no qualifier (because `c` is a variable, not a package).
		return []QualifiedCall{{Token: "sendRaw"}}, nil
	})

	callees, err := g.GetCallees("leyline/methods/SendOp")
	require.NoError(t, err)
	require.NotEmpty(t, callees,
		"GetCallees must resolve c.sendRaw via the *.sendRaw suffix-match fallback in node_defs")
	require.Equal(t, "leyline/methods/SocketClient.sendRaw", callees[0].ID,
		"resolved target must be the unique qualified-form node_defs entry")
}

// TestSQLiteGraph_GetCallees_AmbiguousSuffixSkipped pins the
// disambiguation guard for the suffix-match fallback. If two types
// both define a method of the same bare name, we cannot pick one
// without scope info — so the resolver MUST skip rather than guess.
func TestSQLiteGraph_GetCallees_AmbiguousSuffixSkipped(t *testing.T) {
	dir, err := os.MkdirTemp("", "callees-ambig-*")
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

		INSERT INTO nodes VALUES ('caller', '', 'caller', 1, 0, 1, NULL, NULL);
		INSERT INTO nodes VALUES ('caller/source', 'caller', 'source', 0, 0, 1, NULL, 'x.Close()');
		INSERT INTO node_refs VALUES ('Close', 'caller/source');

		-- Two types both define Close — ambiguous.
		INSERT INTO node_defs VALUES ('Reader.Close', 'reader/Reader.Close');
		INSERT INTO node_defs VALUES ('Writer.Close', 'writer/Writer.Close');
	`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	g, err := OpenSQLiteGraph(dbPath, &api.Topology{}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })

	g.SetCallExtractor(func(_ []byte, _, _ string) ([]QualifiedCall, error) {
		return []QualifiedCall{{Token: "Close"}}, nil
	})

	callees, err := g.GetCallees("caller")
	require.NoError(t, err)
	require.Empty(t, callees,
		"ambiguous *.Close suffix match (2 candidates) must NOT auto-resolve — silent wrong-target is worse than no answer")
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
