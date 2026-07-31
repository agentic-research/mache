package fixturedb

// The mache-standalone table contract, DERIVED not authored.
//
// Every statement below was copied out of `sqlite_master` after running
// ingest.NewSQLiteWriter on an empty path, with the explanatory `--` comments
// (which SQLite stores verbatim in sqlite_master.sql) dropped.
// TestStandaloneSchema_MatchesSQLiteWriter re-derives it from
// internal/ingest/sqlite_writer.go's own schema literal and fails on drift.
//
// This is the shape 13 of the pre-fixturedb test files reproduced exactly —
// while believing, in several cases, that they were modelling ley-line.

// standaloneTables is the mache-projection subset fixtures model, keyed by table
// name.
//
// `_ast`, `_source`, `node_content` and `_imports` are deliberately ABSENT:
// SQLiteWriter does not write them. When a Standalone fixture needs them (the
// cache-hydration path in cmd/cache.go materialises `_ast` onto a mache .db),
// they are created with the LEY-LINE shape from [leylineTables] — those tables
// are ley-line-owned and have exactly one contract, which is the whole point of
// the LLO boundary rule in internal/lint.
var standaloneTables = map[string]string{
	"nodes": `CREATE TABLE nodes (
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
	)`,

	// PRIMARY KEY (token, node_id) WITHOUT ROWID — duplicate rows are DEDUPED,
	// which is the opposite of ley-line. Any COUNT/AVG-over-v_refs rule measures
	// a different quantity here (mache-50e939). There is no column for a call
	// site, a qualifier, or a source: node_id IS the enclosing construct.
	"node_refs": `CREATE TABLE node_refs (
		token TEXT,
		node_id TEXT,
		PRIMARY KEY (token, node_id)
	) WITHOUT ROWID`,

	"node_defs": `CREATE TABLE node_defs (
		token TEXT,
		node_id TEXT,
		PRIMARY KEY (token, node_id)
	) WITHOUT ROWID`,

	"file_index": `CREATE TABLE file_index (
		path TEXT PRIMARY KEY,
		mod_time INTEGER NOT NULL,
		size INTEGER NOT NULL
	)`,

	"_index_coverage": `CREATE TABLE _index_coverage (
		source_id TEXT NOT NULL,
		producer TEXT NOT NULL,
		fidelity TEXT NOT NULL,
		indexed_at INTEGER NOT NULL,
		complete INTEGER NOT NULL,
		PRIMARY KEY (source_id, producer)
	) WITHOUT ROWID`,
}

var standaloneIndexes = map[string]string{
	"idx_parent_name": `CREATE INDEX idx_parent_name ON nodes(parent_id, name)`,
	"idx_source_file": `CREATE INDEX idx_source_file ON nodes(source_file)`,
}

// standaloneViews are the PERSISTENT v_defs / v_refs SQLiteWriter installs.
//
// They matter because ensureCanonicalViews relies on TEMP objects SHADOWING
// same-named main-schema objects for the current connection (see its doc
// comment). A fixture that omits these never exercises the shadowing, so a
// regression in it would be invisible — exactly the hidden-parameter problem
// this package removes.
var standaloneViews = map[string]string{
	"v_defs": `CREATE VIEW v_defs AS
		SELECT token, node_id, 'mention' AS fidelity FROM node_defs`,

	"v_refs": `CREATE VIEW v_refs AS
		SELECT node_id AS referrer_node_id,
		       token,
		       NULL  AS target_node_id,
		       NULL  AS ref_uri,
		       NULL  AS ref_line,
		       'mention' AS fidelity
		FROM node_refs`,
}

// standaloneTableOrder is creation order.
var standaloneTableOrder = []string{
	"nodes", "node_refs", "node_defs", "file_index", "_index_coverage",
}
