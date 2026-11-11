package graph

import (
	"database/sql"
	"log"
	"maps"
	"slices"
	"strings"
)

// ProjectionSchemaVersionKey is the `_meta` row ley-line-open stamps with the
// shape of the projection it wrote.
const ProjectionSchemaVersionKey = "projection_schema_version"

// knownProjections are the ley-line-open projection shapes mache has been
// checked against. Absence from this map is not a claim that a projection is
// broken — only that nobody has looked.
//
// An arena that declares NOTHING is the common case, not an error. The `_meta`
// channel does not exist in v0.18.0 / v0.18.1 / v0.18.2, so every currently
// RELEASED leyline writes an arena with no projection row at all; verified by
// inspecting a real v0.18.2 arena, whose `_meta` carries source_root,
// parse_time, ir_schema_version, extraction_epoch, injection_epoch and
// query_set_epoch and nothing else. mache's own `mache build` output has no
// `_meta` either.
var knownProjections = map[string]string{
	// The shape the v0.18.x releases write. They do not DECLARE it — the
	// _meta channel did not exist yet — but ley-line-open's main stamped it
	// between v0.18.2 and v0.19.0, so arenas from that window say so.
	"projection-v2": "pre-v0.19.0: parent_id stored, spans only on _ast",
	// The pinned shape for leyline v0.19.x. nodes.parent_id becomes
	// GENERATED; node_defs/node_refs gain their own node_kind + span columns;
	// _ast gains blob_ord. Every change is ADDITIVE except the parent_id
	// conversion, and nothing is removed — verified by re-deriving
	// internal/fixturedb's mirror from the released binary.
	//
	// The generated column is TRANSITIONAL, not the steady state: LLO plans to
	// delete it in ley-line-open-17c271 and return parent_id to a STORED
	// integer FK, because the derivation only means anything while ids are
	// path-shaped. mache probes the column at runtime rather than keying on
	// this version, so that reversal needs no change here (mache-bc6ca3).
	"projection-v4": "leyline v0.19.x: parent_id derived; spans on node_defs/node_refs; _ast.blob_ord",
	// The shape leyline v0.20.x writes (v0.20.0 introduced it): nodes
	// keyed by integer nid with interned names and kinds, paths derived by
	// LLO's v_node_name rather than stored, _ast/node_defs/node_refs keyed by
	// nid. Every mache reader handles it through internal/lloschema, checked
	// against real parses of one tree in both shapes (mache-90cb9f).
	"projection-v6": "leyline v0.20.x: integer nids, derived paths, interned names and kinds",
	// The shape leyline v0.21.x writes: projection-v6 plus two tables,
	// with every v6 table unchanged (fixturedb's mirror re-derived from the
	// released binary). _tree(path, mode, blob_hash, mtime, size) lists every
	// entry, explicit directories included, while _source lists only the files
	// a parser projected, with _source.content_hash = _tree.blob_hash;
	// _parse_diagnostics records a file that parsed with errors (projected=1)
	// or not at all (binary or oversized, projected=0). The readers mache had
	// for v6 read v7 unchanged.
	"projection-v7": "leyline v0.21.x: v6 plus _tree (every entry) and _parse_diagnostics",
	// v7 minus capnp_blobs and _ast_blob, LLO's second stored copy of the
	// syntax tree, which mache never read. No leyline release writes it: it
	// shipped only inside v0.22.0's move to v9, so mache has seen it only as
	// the step between (mache-72668d).
	"projection-v8": "unreleased: v7 minus capnp_blobs and _ast_blob",
	// The shape leyline v0.22.x and v0.23.x write: v8 with the merkle edges
	// interned. node_content gains content_id (arena-local, not an identity);
	// node_edge(parent_id, ordinal, child_id, field) holds the edges; and
	// node_child is a read-only VIEW over it with v8's columns. A plain read of
	// node_child runs unchanged, but SQLite materializes the whole view on the
	// right of a LEFT JOIN, so the one such reader joins node_edge on v9
	// (lloschema.FieldChildJoins). Checked against sqlite_master of one tree
	// parsed by v0.21.1 and v0.22.0: nothing else differs.
	"projection-v9": "leyline v0.22-v0.23: v8 with node_edge interning node_child, which becomes a view",
	// The shape leyline v0.24.x-v0.25.x write: v9 with _ast, node_defs and
	// node_refs carrying node_content's content_id where they carried the
	// 32-byte node_hash (ley-line-open-738dd9). node_content.node_hash stays
	// the identity and is byte-identical across the change; mache reads it
	// through lloschema.NodeHashExpr. Checked against sqlite_master of one tree
	// parsed by v0.23.0 and v0.24.0: those three columns, the _ast index, and
	// three LLO node_hash views are the whole difference.
	"projection-v10": "leyline v0.24-v0.25: v9 with occurrence rows keyed to node_content by content_id",
	// v10 with no table or column change: _meta.derived_from becomes
	// "v2 <storeRoot> <parserId> <projection>", and the daemon wire's
	// tree_root becomes a parser-free store_root (ley-line-open-73c6ee,
	// mache-107423). Shipped only inside v0.26.0's move to v12.
	"projection-v11": "unreleased alone: v10 with derived_from v2 (storeRoot)",
	// The shape leyline v0.26.0-v0.28.0 write: v11 with node_hash keyed
	// (IR merkle-ast-v3, so every hash value differs from v2), raw_kind and
	// lang moved off node_content to the occurrence, and node_defs/node_refs
	// gaining kind_id (mache-108e26). A fielded child's grammar kind comes
	// from its occurrence (lloschema.FieldChildJoins). Checked against
	// sqlite_master of one tree parsed by v0.25.0 and v0.26.0: those columns
	// and the two views' kind_id are the whole difference.
	"projection-v12": "leyline v0.26-v0.28: keyed node_hash (merkle-ast-v3); kind and language per occurrence",
	// THE PINNED SHAPE as of leyline v0.29.0: v12 with every comment an _ast
	// and nodes occurrence (extraction epoch 7, ley-line-open-aa1634). No
	// table or column moved; two invariants a v12 reader relied on did: a
	// comment has no node_edge from its parent, and a file's comments are
	// numbered after its code, so nid order is document order among code
	// only. mache folds comments: the file index holds them apart from the
	// nodes, v_ast gives them no subtree and marks them is_comment, listings
	// and path resolution skip them, and a doc comment extends the construct
	// below it (mache-1e3d11). Checked against sqlite_master of one tree
	// parsed by v0.28.0 and v0.29.0: identical.
	"projection-v13": "the pinned shape: v12 with comments as _ast occurrences (extraction epoch 7)",
}

// ProjectionVersion returns the projection shape an artifact declares and
// whether it declares one at all.
//
// A missing `_meta` table, a missing row and an empty value are all reported
// the same way — undeclared — because a reader cannot act on the difference:
// each means this artifact will not tell you its shape.
func ProjectionVersion(db *sql.DB) (string, bool) {
	var v string
	if err := db.QueryRow(
		`SELECT value FROM _meta WHERE key = ?`, ProjectionSchemaVersionKey).Scan(&v); err != nil {
		return "", false
	}
	return v, v != ""
}

// warnUnknownProjection logs when an artifact declares a projection shape mache
// has never been checked against.
//
// mache validates the leyline BINARY by an exact version pin but, until this,
// validated the ARTIFACT not at all. The pin alone is demonstrably not enough:
// ley-line-open's projection-v4 branch reports version 0.18.2 — byte-identical
// to the release whose schema it changes — so the pin accepts it as the pinned
// binary (ley-line-open-cbd3c9). Reading the shape the FILE declares closes that
// gap, because it is a property of the artifact rather than of whatever claimed
// to produce it, and it holds for a .db that was copied, cached, or built by
// somebody else's leyline entirely.
//
// It warns rather than refusing, deliberately. Every projection bump so far has
// been additive for readers, and mache's readers already probe per column via
// ColumnExists rather than assuming a shape, so refusing would turn arenas mache
// can in fact read into arenas it rejects — including for library callers
// assembling many projections at once, where a single newer shard would sink the
// whole corpus. When a projection appears that mache genuinely CANNOT read, that
// is the moment to refuse it by name, the way RequireProps refuses exactly the
// stale-props case and nothing else.
func warnUnknownProjection(db *sql.DB, dbPath string) {
	v, declared := ProjectionVersion(db)
	if !declared {
		return // pre-_meta arena or mache's own projection: the common case
	}
	if _, ok := knownProjections[v]; ok {
		return
	}
	log.Printf("warn: %s declares projection %q, which this mache has not been checked "+
		"against (known: %s) — reads may be silently incomplete if the shape changed",
		dbPath, v, strings.Join(slices.Sorted(maps.Keys(knownProjections)), ", "))
}
