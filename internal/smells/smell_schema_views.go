package smells

import (
	"fmt"

	"github.com/agentic-research/mache/graph"
)

// The boundary between mache's rule vocabulary and ley-line-open's physical
// schema (mache-be17ce).
//
// LLO's projection-v6 (release v0.20.0) replaced path-string node ids with
// integer nids: `_ast.node_id` -> `_ast.nid`, `_ast.node_kind` -> a `kind_id`
// into `kinds`, `_ast.source_id` gone in favour of `nid >> 24`, and
// `nodes.id`/`name`/`parent_id` replaced by `nid`/`name_id`/`parent_nid`.
// Before this file, fourteen smell rules and ~25 Go files named those columns
// directly, so the producer's storage layout was mache's query language and a
// producer release rewrote every one of them.
//
// The fix is NOT a compatibility shim. Rendering the old path-string id back
// out of v6 would reinstate exactly the ~1 GB of path text across six b-trees
// that v6 exists to remove (mache-3688da). What is stable is the VOCABULARY,
// not the representation:
//
//	v_ast    node_id, source_id, node_kind, start_byte … end_col, node_hash
//	v_nodes  node_id, parent_id, name, kind, source_file
//
// `node_id` is whatever the producer uses — a path string on v4, an integer on
// v6 — and consumers must treat it as OPAQUE. `source_id` stays the
// repo-relative source path on both, because rules scope and REPORT by it;
// that costs one join against `_source`, which has one row per file, not per
// node.
//
// LLO's own `v_node_path` view is deliberately not used here. It is a
// RECURSIVE CTE walking parent_nid to the root, so it belongs at the edge
// where a human-readable path is rendered, never inside a join.
//
// TEMP views for the same reasons as EnsureCanonicalViews: SQLiteGraph opens
// read-only so persistent DDL fails, and the body depends on what the producer
// wrote, so it must be recomputed per connection rather than frozen on disk.

// ensureSchemaViews installs v_ast and v_nodes over whichever projection
// schema the producer wrote.
//
// A backend with no `_ast` gets no `v_ast`, which is what makes the rules'
// `Requires: ["v_ast"]` skip them instead of failing at query time — the same
// degradation the physical `Requires: ["_ast"]` gave before.
func ensureSchemaViews(qg graph.RefsQuerier) error {
	astBody, err := astViewBody(qg)
	if err != nil {
		return err
	}
	nodesBody, err := nodesViewBody(qg)
	if err != nil {
		return err
	}

	stmts := []string{"DROP VIEW IF EXISTS temp.v_ast", "DROP VIEW IF EXISTS temp.v_nodes"}
	if astBody != "" {
		stmts = append(stmts, "CREATE TEMP VIEW v_ast AS "+astBody)
	}
	if nodesBody != "" {
		stmts = append(stmts, "CREATE TEMP VIEW v_nodes AS "+nodesBody)
	}
	for _, s := range stmts {
		rows, qerr := qg.QueryRefs(s)
		if qerr != nil {
			return fmt.Errorf("ensure schema views: %w", qerr)
		}
		_ = rows.Close()
	}
	return nil
}

// astViewBody returns the SELECT for v_ast, or "" when the producer wrote no
// usable `_ast`.
func astViewBody(qg graph.RefsQuerier) (string, error) {
	v6, err := TableHasColumn(qg, "_ast", "nid")
	if err != nil {
		return "", fmt.Errorf("probe _ast.nid: %w", err)
	}
	if v6 {
		// nid >> 24 is the interned file id (LLO's nid = file_id<<24 | ordinal),
		// and _source.file_id is UNIQUE, so this join is one row per file.
		return `SELECT a.nid                  AS node_id,
		       s.id                  AS source_id,
		       k.raw_kind            AS node_kind,
		       a.start_byte, a.end_byte,
		       a.start_row, a.start_col, a.end_row, a.end_col,
		       a.node_hash
		FROM _ast a
		JOIN kinds   k ON k.kind_id = a.kind_id
		JOIN _source s ON s.file_id = (a.nid >> 24)`, nil
	}

	v4, err := TableHasColumn(qg, "_ast", "node_id")
	if err != nil {
		return "", fmt.Errorf("probe _ast.node_id: %w", err)
	}
	if !v4 {
		return "", nil
	}
	hasHash, err := TableHasColumn(qg, "_ast", "node_hash")
	if err != nil {
		return "", fmt.Errorf("probe _ast.node_hash: %w", err)
	}
	hashExpr := "NULL"
	if hasHash {
		hashExpr = "node_hash"
	}
	return `SELECT node_id, source_id, node_kind,
	       start_byte, end_byte, start_row, start_col, end_row, end_col,
	       ` + hashExpr + ` AS node_hash
	FROM _ast`, nil
}

// nodesViewBody returns the SELECT for v_nodes, or "" when there is no `nodes`
// table to project.
//
// `parent_id` is the one column whose MEANING, not just spelling, moved. On v4
// it is a generated prefix of the path-string id; on v6 the producer stores
// parent_nid outright. Both answer "which node encloses this one", which is
// the only thing consumers may ask — mache-93e84b already isolated the last
// rule that inferred it from the id's shape, for this change.
func nodesViewBody(qg graph.RefsQuerier) (string, error) {
	v6, err := TableHasColumn(qg, "nodes", "nid")
	if err != nil {
		return "", fmt.Errorf("probe nodes.nid: %w", err)
	}
	if v6 {
		// v_node_name is LLO's own, and reimplementing it here is the
		// coupling this file exists to remove: a node with no interned
		// name_id is named after its kind plus an ordinal among like-kinded
		// siblings (`function_declaration_1`), which is the v4 scheme mache
		// depends on. Unlike v_node_path it is a flat correlated select, not
		// a recursive walk, so it is safe in a join.
		return `SELECT n.nid        AS node_id,
		       n.parent_nid AS parent_id,
		       vn.name      AS name,
		       n.kind       AS kind,
		       n.source_file
		FROM nodes n
		JOIN v_node_name vn ON vn.nid = n.nid`, nil
	}

	v4, err := TableHasColumn(qg, "nodes", "id")
	if err != nil {
		return "", fmt.Errorf("probe nodes.id: %w", err)
	}
	if !v4 {
		return "", nil
	}
	return `SELECT id AS node_id, parent_id, name, kind, source_file FROM nodes`, nil
}
