package fixturedb

// The ley-line-open projection-v6 table contract, DERIVED not authored.
//
// Every statement below was copied out of `sqlite_master` after running a
// v0.20.0 ley-line binary on a one-file Go corpus:
//
//	$ leyline parse ./src -o out.db
//	$ sqlite3 out.db "SELECT sql FROM sqlite_master ORDER BY type, name"
//
// Same derivation, and same reason, as schema_leyline.go: hand-writing it
// reproduces the bug this package exists to remove — one wrong spelling
// instead of thirty-four.
//
// SQL COMMENTS ARE STRIPPED, and must be. LLO documents columns inline and
// those comments contain BACKTICKS (`ley-line-open-17c271`), which cannot
// appear inside a Go raw string literal at all. Nothing is lost: normalizeDDL
// discards `--` comments on both sides before comparing.
//
// WHY A SECOND MIRROR RATHER THAN REPLACING THE FIRST. mache reads artifacts
// produced by several leyline versions — lloschema.resolveIdentity exists
// precisely because a db carries its own answer — so fixtures must be able to
// model both. schema_leyline.go stays the v4 model and stays conformance-
// checked against the pin; this one models v6.
//
// CONFORMANCE STATUS, stated plainly rather than implied: while the pin is
// v0.19.1 this mirror is NOT verified against a binary in CI, because CI has
// no v0.20.0 binary to verify it against. TestLeylineV6Schema_MatchesBinary
// checks it whenever one IS resolvable (locally, or via
// MACHE_LEYLINE_BINARY), and the pin bump turns the check on permanently by
// making v6 the pinned shape. Until then, treat a difference found by that
// test as this file's re-derivation worklist — the arrangement mache-cc1a70
// established for validating against an LLO candidate.

// NO PRODUCER USES THIS YET, and adding one has a trap worth knowing before
// you start: emit.go branches on `if e.b.producer != Leyline`, so a third
// producer silently falls into the STANDALONE arm and writes v4 mache-schema
// rows into a v6 table set. Make those switches exhaustive and loud on an
// unhandled producer FIRST; a fixture that quietly models the wrong shape is
// the hidden-parameter failure this whole package exists to remove
// (mache-7555da).
//
// What this file is for in the meantime: it is the re-derivation worklist the
// pin bump needs, verified against a real v0.20.0 binary NOW rather than
// discovered during the bump.

// leylineV6SchemaVersion is the ley-line-open release these statements were
// derived from.
const leylineV6SchemaVersion = "v0.20.0"

// leylineV6Tables is the ley-line-owned subset fixtures model, keyed by table
// name. Includes the four INTERNING tables v6 added — names, kinds, dirs,
// files — because a v6 fixture cannot express a node's name or kind without
// them: `nodes` holds only name_id and kind_id.
var leylineV6Tables = map[string]string{
	"_ast": `CREATE TABLE _ast (
    nid INTEGER PRIMARY KEY,
    kind_id INTEGER NOT NULL,
    start_byte INTEGER NOT NULL,
    end_byte INTEGER NOT NULL,
    start_row INTEGER NOT NULL,
    start_col INTEGER NOT NULL,
    end_row INTEGER NOT NULL,
    end_col INTEGER NOT NULL,
    node_hash BLOB REFERENCES node_content(node_hash)
)`,

	"_imports": `CREATE TABLE _imports (
    alias TEXT NOT NULL,
    path TEXT NOT NULL,
    source_id TEXT NOT NULL
)`,

	"_source": `CREATE TABLE _source (
    id TEXT PRIMARY KEY,
    language TEXT NOT NULL,
    content BLOB,
    path TEXT,
    content_hash BLOB,
    file_id INTEGER UNIQUE
)`,

	"dirs": `CREATE TABLE dirs (
    dir_id INTEGER PRIMARY KEY,
    parent_dir_id INTEGER,
    name_id INTEGER NOT NULL,
    CHECK (dir_id = 1 OR parent_dir_id IS NOT NULL),
    UNIQUE(parent_dir_id, name_id)
)`,

	"files": `CREATE TABLE files (
    file_id INTEGER PRIMARY KEY,
    dir_id INTEGER NOT NULL,
    name_id INTEGER NOT NULL,
    UNIQUE(dir_id, name_id)
)`,

	"kinds": `CREATE TABLE kinds (
    kind_id INTEGER PRIMARY KEY,
    lang TEXT NOT NULL,
    raw_kind TEXT NOT NULL,
    UNIQUE(lang, raw_kind)
)`,

	"names": `CREATE TABLE names (
    name_id INTEGER PRIMARY KEY,
    text TEXT NOT NULL UNIQUE
)`,

	"node_child": `CREATE TABLE node_child (
    parent_hash BLOB    NOT NULL REFERENCES node_content(node_hash),
    ordinal     INTEGER NOT NULL,
    child_hash  BLOB    NOT NULL REFERENCES node_content(node_hash),
    field       TEXT,
    PRIMARY KEY (parent_hash, ordinal)
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

	"node_defs": `CREATE TABLE node_defs (
    token TEXT NOT NULL,
    nid INTEGER NOT NULL,
    container_nid INTEGER,
    node_kind TEXT,
    start_byte INTEGER,
    end_byte INTEGER,
    start_row INTEGER,
    start_col INTEGER,
    end_row INTEGER,
    end_col INTEGER,
    canonical_kind TEXT,
    node_hash BLOB REFERENCES node_content(node_hash)
)`,

	"node_refs": `CREATE TABLE node_refs (
    token TEXT NOT NULL,
    nid INTEGER NOT NULL,
    container_nid INTEGER,
    node_kind TEXT,
    start_byte INTEGER,
    end_byte INTEGER,
    start_row INTEGER,
    start_col INTEGER,
    end_row INTEGER,
    end_col INTEGER,
    qualifier TEXT,
    node_hash BLOB REFERENCES node_content(node_hash)
)`,

	"nodes": `CREATE TABLE nodes (
    nid INTEGER PRIMARY KEY,
    parent_nid INTEGER,
    name_id INTEGER,
    kind_id INTEGER,
    kind INTEGER NOT NULL,
    ord INTEGER NOT NULL DEFAULT 0,
    size INTEGER DEFAULT 0,
    mtime INTEGER NOT NULL,
    record_id TEXT,
    record TEXT,
    source_file TEXT
)`,
}

// leylineV6Views are LLO's OWN views, and modelling them is not optional.
// v_node_name encodes the naming rule mache depends on — a node with no
// interned name is named after its kind plus an ordinal among like-kinded
// siblings — and lloschema's v_nodes joins it rather than reimplementing it.
// A fixture without it cannot build v_nodes at all.
var leylineV6Views = map[string]string{
	"v_node_name": `CREATE VIEW v_node_name AS
SELECT n.nid AS nid,
       CASE
         WHEN n.name_id IS NOT NULL THEN (SELECT text FROM names WHERE name_id = n.name_id)
         WHEN (SELECT COUNT(*) FROM nodes s
                WHERE s.parent_nid = n.parent_nid AND s.kind_id = n.kind_id) > 1
           THEN (SELECT raw_kind FROM kinds k WHERE k.kind_id = n.kind_id)
                || '_' ||
                (SELECT COUNT(*) FROM nodes s
                  WHERE s.parent_nid = n.parent_nid AND s.kind_id = n.kind_id
                    AND s.ord < n.ord)
         ELSE (SELECT raw_kind FROM kinds k WHERE k.kind_id = n.kind_id)
       END AS name
FROM nodes n`,

	"v_node_path": `CREATE VIEW v_node_path AS
WITH RECURSIVE walk(nid, path, cursor) AS (
  SELECT n.nid, '', n.nid FROM nodes n
  UNION ALL
  SELECT w.nid,
         CASE WHEN v.name = '' THEN w.path
              WHEN w.path = '' THEN v.name
              ELSE v.name || '/' || w.path END,
         p.parent_nid
  FROM walk w
  JOIN nodes p ON p.nid = w.cursor
  JOIN v_node_name v ON v.nid = w.cursor
)
SELECT nid, path FROM walk WHERE cursor IS NULL`,
}

// leylineV6Indexes matter for the same reason the v4 ones do: a fixture
// missing an index plans queries differently from a real .db.
var leylineV6Indexes = map[string]string{
	"idx_ast_node_hash": `CREATE INDEX idx_ast_node_hash ON _ast(node_hash)`,

	"idx_defs_canonical_kind": `CREATE INDEX idx_defs_canonical_kind ON node_defs(canonical_kind) WHERE canonical_kind IS NOT NULL`,

	"idx_defs_container": `CREATE INDEX idx_defs_container ON node_defs(container_nid) WHERE container_nid IS NOT NULL`,

	"idx_defs_node": `CREATE INDEX idx_defs_node ON node_defs(nid)`,

	"idx_defs_token": `CREATE INDEX idx_defs_token ON node_defs(token)`,

	"idx_imports_source": `CREATE INDEX idx_imports_source ON _imports(source_id)`,

	"idx_parent_kind_ord": `CREATE INDEX idx_parent_kind_ord ON nodes(parent_nid, kind_id, ord)`,

	"idx_refs_container": `CREATE INDEX idx_refs_container ON node_refs(container_nid) WHERE container_nid IS NOT NULL`,

	"idx_refs_node": `CREATE INDEX idx_refs_node ON node_refs(nid)`,

	"idx_refs_token": `CREATE INDEX idx_refs_token ON node_refs(token)`,
}
