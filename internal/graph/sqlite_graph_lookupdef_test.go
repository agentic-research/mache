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

// TestSQLiteGraph_GetCallees_RequiresLangProperty pins the bug from
// bead mache-d28eb1: SQLiteGraph.GetCallees reads the construct node's
// `lang` Property to pick an extractor. If the property is missing,
// langName="" → grammar lookup fails → empty callees. Pre-fix,
// `mache build` produced .db files where this property was wiped by
// the two-pass write pattern in engine.processNode (SQLiteWriter
// .GetNode returned no Properties, so the second pass overwrote them
// with nil).
//
// This test uses the construct node's record JSON directly to assert
// the resolver path reads `lang` and only resolves callees when it's
// present.
func TestSQLiteGraph_GetCallees_RequiresLangProperty(t *testing.T) {
	dir, err := os.MkdirTemp("", "callees-lang-*")
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

		-- Construct dir with lang property base64-encoded ("go" → "Z28=")
		INSERT INTO nodes VALUES ('caller', '', 'caller', 1, 0, 1, NULL, '{"lang":"Z28="}');
		INSERT INTO nodes VALUES ('caller/source', 'caller', 'source', 0, 30, 1, NULL, 'func f() { target() }');
		INSERT INTO nodes VALUES ('target', '', 'target', 1, 0, 1, NULL, '{"lang":"Z28="}');
		INSERT INTO node_defs VALUES ('target', 'target');
	`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	g, err := OpenSQLiteGraph(dbPath, &api.Topology{}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })

	g.SetCallExtractor(func(_ []byte, _, langName string) ([]QualifiedCall, error) {
		// Production extractor returns nil for empty langName because
		// it can't pick a grammar. This stub mirrors that behavior so
		// the test catches "lang missing → no callees" regressions.
		if langName == "" {
			return nil, nil
		}
		return []QualifiedCall{{Token: "target"}}, nil
	})

	callees, err := g.GetCallees("caller")
	require.NoError(t, err)
	require.Len(t, callees, 1,
		"lang property present + extractor returns calls → GetCallees must resolve")
	require.Equal(t, "target", callees[0].ID)
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

// TestSQLiteGraph_DefsMap_SQLFallback regression-guards bead
// mache-655e98: DefsMap() was reading ONLY in-memory g.defs, so on
// pre-built .db files where definitions live in the node_defs SQL
// table, it returned an empty map. Downstream tools that depend on
// DefsMap() (get_impact, get_architecture, and the find_callees
// resolver fallback) silently returned "no definition found" for
// every symbol on every corpus.
//
// Surfaced 2026-05-19 by SB-01's matrix runner: get_impact returned
// the same 51-byte error envelope ({"symbol":...,"error":"no
// definition found"}) regardless of corpus (mache, rosary, LLO) or
// language (Go, Rust) when run against SQLiteGraph. MemoryStore
// returned rich impact data on the same fixtures.
//
// This test sets up a node_defs row WITHOUT any AddDef call and
// asserts DefsMap returns it — exercising the SQL-fallback path
// that mirrors SearchDefs and LookupDef.
func TestSQLiteGraph_DefsMap_SQLFallback(t *testing.T) {
	dir, err := os.MkdirTemp("", "defsmap-sqlfallback-*")
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

		INSERT INTO node_defs VALUES ('NewEngine', 'ingest/functions/NewEngine');
		INSERT INTO node_defs VALUES ('Validate', 'auth/functions/Validate');
		INSERT INTO node_defs VALUES ('Engine.Ingest', 'ingest/functions/Engine.Ingest');
	`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	g, err := OpenSQLiteGraph(dbPath, &api.Topology{}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })

	got := g.DefsMap()
	require.Len(t, got, 3, "DefsMap must hydrate all node_defs rows on pre-built .db")
	require.Equal(t, []string{"ingest/functions/NewEngine"}, got["NewEngine"])
	require.Equal(t, []string{"auth/functions/Validate"}, got["Validate"])
	require.Equal(t, []string{"ingest/functions/Engine.Ingest"}, got["Engine.Ingest"])
}

// TestSQLiteGraph_DefsMap_MergesInMemoryAndSQL pins that AddDef
// entries layer on top of the SQL-fallback hydration without
// dropping either source. Mirrors TestSQLiteGraph_LookupDef_InMemoryWinsOverSQL
// for the bulk-snapshot path.
func TestSQLiteGraph_DefsMap_MergesInMemoryAndSQL(t *testing.T) {
	dir, err := os.MkdirTemp("", "defsmap-merge-*")
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
		INSERT INTO node_defs VALUES ('OnlyInSQL', 'sql/OnlyInSQL');
		INSERT INTO node_defs VALUES ('InBoth', 'sql/InBoth');
	`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	g, err := OpenSQLiteGraph(dbPath, &api.Topology{}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })

	require.NoError(t, g.AddDef("OnlyInMemory", "mem/OnlyInMemory"))
	require.NoError(t, g.AddDef("InBoth", "mem/InBoth"))

	got := g.DefsMap()
	require.Contains(t, got, "OnlyInSQL", "SQL-only def must appear in DefsMap")
	require.Contains(t, got, "OnlyInMemory", "in-memory-only def must appear in DefsMap")
	require.Contains(t, got, "InBoth", "def present in both sources must appear in DefsMap")
	// In-memory wins on conflict, matching LookupDef precedence.
	require.Equal(t, []string{"mem/InBoth"}, got["InBoth"],
		"on token collision, in-memory entry must win (matches LookupDef precedence)")
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
