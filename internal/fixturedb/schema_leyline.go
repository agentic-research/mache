package fixturedb

// The ley-line-open table contract, DERIVED not authored.
//
// Every statement below was copied verbatim out of `sqlite_master` after
// running the PINNED ley-line binary on a two-file corpus:
//
//	$ leyline parse ./src -o out.db
//	$ sqlite3 out.db "SELECT sql FROM sqlite_master ORDER BY type, name"
//
// Hand-writing it would reproduce the bug this package exists to remove — one
// wrong spelling instead of thirty-four. TestLeylineSchema_MatchesPinnedBinary
// (schema_leyline_conformance_test.go) re-runs that derivation and fails on any
// drift, so a ley-line release that changes shape breaks HERE, loudly, instead
// of silently flipping which arm of ensureCanonicalViews the whole test suite
// exercises.
//
// PATCH RELEASES CHANGE THIS SCHEMA. The pin must stay exact; see
// reference_leyline_noderefs_parity_gap.

// leylineSchemaVersion is the ley-line-open release these statements were
// derived from. The conformance test asserts the pinned binary still reports it.
const leylineSchemaVersion = "v0.15.1"

// leylineTables is the ley-line-owned subset fixtures model, keyed by table
// name. Deliberately not every table ley-line writes — `capnp_blobs`,
// `query_blobs`, `source_blobs`, `_queries`, `_meta`, `_ast_pointer` and
// `_file_index` carry no signal any smell rule or canonical view reads, and
// modelling them would only create more surface to drift.
//
// The values are byte-identical to the pinned producer's `sqlite_master.sql`.
var leylineTables = map[string]string{
	"nodes": `CREATE TABLE nodes (
    id TEXT PRIMARY KEY,
    parent_id TEXT,
    name TEXT NOT NULL,
    kind INTEGER NOT NULL,
    size INTEGER DEFAULT 0,
    mtime INTEGER NOT NULL,
    record_id TEXT,
    record TEXT,
    source_file TEXT
)`,

	// NO PRIMARY KEY and NOT `WITHOUT ROWID` — duplicate (token, node_id) rows
	// SURVIVE here. ley-line dual-emits a qualified and a bare row for the same
	// call site (`fmt.Println` with qualifier NULL, `Println` with qualifier
	// 'fmt'), both at the same node_id. Any fixture that adds a primary key
	// silently dedupes rows production keeps.
	"node_defs": `CREATE TABLE node_defs (
    token TEXT NOT NULL,
    node_id TEXT NOT NULL,
    source_id TEXT NOT NULL,
    container_node_id TEXT,
    canonical_kind TEXT
, node_hash BLOB REFERENCES node_content(node_hash))`,

	"node_refs": `CREATE TABLE node_refs (
    token TEXT NOT NULL,
    node_id TEXT NOT NULL,
    source_id TEXT NOT NULL,
    container_node_id TEXT,
    qualifier TEXT
, node_hash BLOB REFERENCES node_content(node_hash))`,

	"_ast": `CREATE TABLE _ast (
    node_id TEXT PRIMARY KEY,
    source_id TEXT NOT NULL,
    node_kind TEXT NOT NULL,
    start_byte INTEGER NOT NULL,
    end_byte INTEGER NOT NULL,
    start_row INTEGER NOT NULL,
    start_col INTEGER NOT NULL,
    end_row INTEGER NOT NULL,
    end_col INTEGER NOT NULL
, node_hash BLOB REFERENCES node_content(node_hash))`,

	"_source": `CREATE TABLE _source (
    id TEXT PRIMARY KEY,
    language TEXT NOT NULL,
    content BLOB,
    path TEXT,
    content_hash BLOB
)`,

	"_imports": `CREATE TABLE _imports (
    alias TEXT NOT NULL,
    path TEXT NOT NULL,
    source_id TEXT NOT NULL
)`,

	"node_content": `CREATE TABLE node_content (
    node_hash BLOB PRIMARY KEY,
    node_tag  INTEGER NOT NULL,
    kind      TEXT    NOT NULL,
    raw_kind  TEXT    NOT NULL,
    lang      TEXT    NOT NULL,
    token     TEXT,
    arity     INTEGER NOT NULL
)`,
}

// leylineIndexes are the indexes the pinned producer creates on the tables in
// [leylineTables]. Included because query plans are part of the shape a fixture
// claims to reproduce, and because the conformance test can then assert on them
// too — an index ley-line drops is a signal, not noise.
var leylineIndexes = map[string]string{
	"idx_ast_node_hash":       `CREATE INDEX idx_ast_node_hash ON _ast(node_hash)`,
	"idx_ast_source":          `CREATE INDEX idx_ast_source ON _ast(source_id)`,
	"idx_defs_canonical_kind": `CREATE INDEX idx_defs_canonical_kind ON node_defs(canonical_kind) WHERE canonical_kind IS NOT NULL`,
	"idx_defs_container":      `CREATE INDEX idx_defs_container ON node_defs(container_node_id) WHERE container_node_id IS NOT NULL`,
	"idx_defs_token":          `CREATE INDEX idx_defs_token ON node_defs(token)`,
	"idx_imports_source":      `CREATE INDEX idx_imports_source ON _imports(source_id)`,
	"idx_parent_name":         `CREATE INDEX idx_parent_name ON nodes(parent_id, name)`,
	"idx_refs_container":      `CREATE INDEX idx_refs_container ON node_refs(container_node_id) WHERE container_node_id IS NOT NULL`,
	"idx_refs_node":           `CREATE INDEX idx_refs_node ON node_refs(node_id)`,
	"idx_refs_token":          `CREATE INDEX idx_refs_token ON node_refs(token)`,
}

// leylineTableOrder is creation order: node_content first because node_defs /
// node_refs / _ast carry REFERENCES into it.
var leylineTableOrder = []string{
	"node_content", "nodes", "node_defs", "node_refs", "_ast", "_source", "_imports",
}
