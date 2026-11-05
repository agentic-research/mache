package fixturedb

// The ley-line-open projection-v6 table contract, DERIVED not authored.
//
// Every statement below was copied out of `sqlite_master` after running a
// v0.20.0 ley-line binary on a one-file Go corpus, and re-verified unchanged
// against v0.21.0, the release the pin moves to. v0.20.1 through v0.20.5 keep
// projection-v6 and change no table; v0.21.0 writes projection-v7, which
// changes none of these and adds _tree and _parse_diagnostics, copied from a
// v0.21.0 parse. v0.21.1 was re-derived the same way and its sqlite_master is
// identical, all 225 objects (mache-40e8e7). v0.22.0 writes projection-v9:
// capnp_blobs and _ast_blob are gone, node_content gains content_id, and
// node_edge holds the edges node_child held, node_child becoming a view over it.
// Those four objects are the whole difference from v0.21.1's sqlite_master
// (mache-72668d). v0.23.0's sqlite_master is identical to v0.22.0's
// (mache-161c63). v0.24.0 writes projection-v10: _ast, node_defs and
// node_refs carry content_id where they carried node_hash,
// idx_ast_node_hash becomes idx_ast_content, and three node_hash views are
// added; nothing else differs (mache-99324b):
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
// model both. This one models the nid-keyed shape at the pin, projection-v10,
// and
// TestLeylineSchema_MatchesPinnedBinary checks it against the pinned binary.
// schema_leyline.go stays the v4 model, for artifacts the previous pin built,
// checked against its own binary by TestLeylineV4Schema_MatchesItsBinary.

// The LeylineV6 producer (dialect_v6.go) builds fixtures in this shape.
//
// leylineV6SchemaVersion is the ley-line-open release these statements were
// derived from.
const leylineV6SchemaVersion = "v0.25.0"

// leylineV6Tables is the ley-line-owned subset fixtures model, keyed by table
// name. Includes the four INTERNING tables v6 added — names, kinds, dirs,
// files — because a v6 fixture cannot express a node's name or kind without
// them: `nodes` holds only name_id and kind_id.
var leylineV6Tables = map[string]string{
	// The parsed bytes of every _source row, under _source.content_hash;
	// _source.content is NULL. The only correct source text: node offsets
	// refer to these bytes, not to the file as it is now (mache-46c59d).
	"source_blobs": `CREATE TABLE source_blobs (
    blob_hash BLOB PRIMARY KEY,
    blob_bytes BLOB NOT NULL,
    byte_len INTEGER GENERATED ALWAYS AS (length(blob_bytes)) STORED
)`,

	// projection-v7 (v0.21.0): every entry of the tree, explicit directories
	// included (mode 0o40000, NULL blob_hash), closed under parents. _source
	// lists only the files a parser projected, with content_hash = blob_hash.
	"_tree": `CREATE TABLE _tree (
    path      TEXT PRIMARY KEY,
    mode      INTEGER NOT NULL,
    blob_hash BLOB,
    mtime     INTEGER,
    size      INTEGER
) WITHOUT ROWID`,
	// projection-v7 (v0.21.0): a file that parsed with errors (projected=1)
	// or was not projected at all, being binary or oversized (projected=0).
	"_parse_diagnostics": `CREATE TABLE _parse_diagnostics (
    path       TEXT PRIMARY KEY,
    blob_hash  BLOB NOT NULL,
    projected  INTEGER NOT NULL,
    start_byte INTEGER,
    end_byte   INTEGER,
    start_row  INTEGER,
    start_col  INTEGER,
    message    TEXT NOT NULL
) WITHOUT ROWID`,
	"_ast": `CREATE TABLE _ast (
    nid INTEGER PRIMARY KEY,
    kind_id INTEGER NOT NULL,
    start_byte INTEGER NOT NULL,
    end_byte INTEGER NOT NULL,
    start_row INTEGER NOT NULL,
    start_col INTEGER NOT NULL,
    end_row INTEGER NOT NULL,
    end_col INTEGER NOT NULL,
    content_id INTEGER REFERENCES node_content(content_id)
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

	// projection-v9 (v0.22.0): content_id numbers the rows for node_edge. It
	// is arena-local, not an identity; node_hash stays the FK target.
	"node_content": `CREATE TABLE node_content (
    content_id INTEGER PRIMARY KEY,
    node_hash  BLOB    NOT NULL UNIQUE,
    node_tag   INTEGER NOT NULL,
    kind       TEXT    NOT NULL,
    raw_kind   TEXT    NOT NULL,
    lang       TEXT    NOT NULL,
    token      TEXT,
    arity      INTEGER NOT NULL
)`,

	// projection-v9: the merkle edges, interned on content ids. node_child is
	// now a view over this table (leylineV6Views).
	"node_edge": `CREATE TABLE node_edge (
    parent_id INTEGER NOT NULL REFERENCES node_content(content_id),
    ordinal   INTEGER NOT NULL,
    child_id  INTEGER NOT NULL REFERENCES node_content(content_id),
    field     TEXT,
    PRIMARY KEY (parent_id, ordinal)
) WITHOUT ROWID`,

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
    content_id INTEGER REFERENCES node_content(content_id)
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
    content_id INTEGER REFERENCES node_content(content_id)
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

// leylineV6Unmodelled are the objects the pinned binary creates that fixtures
// deliberately do not model: LLO's own storage and bookkeeping, which mache
// never reads. The conformance test refuses any object that is in neither this
// set nor the mirror, so a table LLO adds is a decision someone makes, not a
// gap nobody sees (projection-v7's _tree and _parse_diagnostics went unnoticed
// for exactly that reason until the v0.21.0 pin).
var leylineV6Unmodelled = map[string]string{
	"_meta": "projection stamps; graph.ProjectionVersion reads one row, tested on its own",
	// projection-v10's node_hash views. mache reads the hash through
	// lloschema.NodeHashExpr's scalar lookup instead: these are LEFT JOIN
	// views, which SQLite materializes whole on the right of a LEFT JOIN.
	"v_ast_hash":       "LLO's node_hash view over _ast; mache uses lloschema.NodeHashExpr",
	"v_node_defs_hash": "LLO's node_hash view over node_defs; mache uses lloschema.NodeHashExpr",
	"v_node_refs_hash": "LLO's node_hash view over node_refs; mache uses lloschema.NodeHashExpr",
	// v0.25.0 (ley-line-open-80f037 F9): the interning tables are
	// append-only, refusing DELETE and an id-changing UPDATE. Fixtures only
	// insert into them, and mache writes none of them outside fixtures.
	"dirs_append_only_delete":  "LLO's append-only guard on dirs; fixtures only insert",
	"dirs_append_only_update":  "LLO's append-only guard on dirs; fixtures only insert",
	"files_append_only_delete": "LLO's append-only guard on files; fixtures only insert",
	"files_append_only_update": "LLO's append-only guard on files; fixtures only insert",
	"kinds_append_only_delete": "LLO's append-only guard on kinds; fixtures only insert",
	"kinds_append_only_update": "LLO's append-only guard on kinds; fixtures only insert",
	"names_append_only_delete": "LLO's append-only guard on names; fixtures only insert",
	"names_append_only_update": "LLO's append-only guard on names; fixtures only insert",
	"_queries":                 "LLO's tree-sitter query registry",
	"query_blobs":              "LLO's compiled-query store",
}

// leylineV6Views are LLO's OWN views, and modelling them is not optional.
// v_node_name encodes the naming rule mache depends on — a node with no
// interned name is named after its kind plus an ordinal among like-kinded
// siblings — and lloschema's v_nodes joins it rather than reimplementing it.
// A fixture without it cannot build v_nodes at all.
var leylineV6Views = map[string]string{
	// projection-v9: the v8 table's columns over node_edge, so every plain
	// node_child read runs unchanged. Not on the right of a LEFT JOIN:
	// lloschema.FieldChildJoins says why.
	"node_child": `CREATE VIEW node_child AS
SELECT p.node_hash AS parent_hash,
       e.ordinal   AS ordinal,
       c.node_hash AS child_hash,
       e.field     AS field
  FROM node_edge e
  JOIN node_content p ON p.content_id = e.parent_id
  JOIN node_content c ON c.content_id = e.child_id`,

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
	"idx_ast_content": `CREATE INDEX idx_ast_content ON _ast(content_id)`,

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
