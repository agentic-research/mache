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
	// THE PINNED SHAPE as of leyline v0.19.0. nodes.parent_id becomes
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
	"projection-v4": "the pinned shape: parent_id derived; spans on node_defs/node_refs; _ast.blob_ord",
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
