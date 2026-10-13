package lloschema

import (
	"fmt"
	"strconv"
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
func ensureSchemaViews(qg Querier) error {
	astBody, err := ASTSelect(qg)
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
		if err := exec(qg, s); err != nil {
			return fmt.Errorf("ensure schema views: %w", err)
		}
	}
	return nil
}

// ASTSelect returns the SELECT behind v_ast, or "" when the producer wrote no
// usable `_ast`. It is exported for a consumer that cannot rely on the TEMP
// view, which exists only on the connection that installed it: such a
// consumer reads `FROM (<ASTSelect>) a` on whatever connection it gets.
func ASTSelect(qg Querier) (string, error) {
	v6, err := TableHasColumn(qg, "_ast", "nid")
	if err != nil {
		return "", fmt.Errorf("probe _ast.nid: %w", err)
	}
	if v6 {
		// nid >> V6FileShift is the interned file id (LLO's nid =
		// file_id<<24 | ordinal), and _source.file_id is UNIQUE, so the file
		// is one lookup.
		//
		// One table, with the file and the kind as scalar lookups rather
		// than joins. SQLite will not flatten a JOIN that is the right
		// operand of a LEFT JOIN, and rules put v_ast exactly there ("this
		// function, LEFT JOIN its branches"): as a three-way join the view
		// was materialized and scanned whole per outer row, 143 s for
		// cyclomatic_complexity on a 492k-node parse. As one table it
		// flattens, and the inner side is a primary-key range: 0.2 s.
		//
		// subtree_lo/subtree_hi bound the node's descendants: a node d is a
		// descendant of a exactly when a.subtree_lo <= d.node_id <
		// a.subtree_hi. Rules ask "inside this function?" (cyclomatic,
		// duplicate_code), and on v4 they answered it by reading node_id as
		// a path prefix, which an opaque id cannot be (mache-2dc105). v6
		// numbers a file's nodes in preorder, so a subtree is the run of
		// nids after the node that start before it ends: the first later
		// node of the file to start at or after end_byte is the first
		// outside it. That holds for any properly nested parse; the one
		// exception is a zero-width node exactly at end_byte, which only
		// error recovery emits. The subquery is evaluated only for rows that
		// read the column, and it stops at the subtree's end.
		shift := strconv.Itoa(V6FileShift)
		fileEnd := "((a.nid >> " + shift + ") + 1) << " + shift
		return `SELECT a.nid                  AS node_id,
		       (SELECT s.id FROM _source s WHERE s.file_id = (a.nid >> ` + shift + `)) AS source_id,
		       (SELECT k.raw_kind FROM kinds k WHERE k.kind_id = a.kind_id) AS node_kind,
		       a.start_byte, a.end_byte,
		       a.start_row, a.start_col, a.end_row, a.end_col,
		       a.node_hash,
		       a.nid + 1             AS subtree_lo,
		       COALESCE((SELECT m.nid FROM _ast m
		                 WHERE m.nid > a.nid AND m.nid < ` + fileEnd + `
		                   AND m.start_byte >= a.end_byte
		                 ORDER BY m.nid LIMIT 1), ` + fileEnd + `) AS subtree_hi
		FROM _ast a`, nil
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
	// A v4 node_id is its path, so its descendants are the ids under
	// node_id + '/': the range up to node_id + '0', the next byte after '/'.
	// A range rather than LIKE: exact (LIKE folds ASCII case) and a search
	// on the node_id index.
	return `SELECT node_id, source_id, node_kind,
	       start_byte, end_byte, start_row, start_col, end_row, end_col,
	       ` + hashExpr + ` AS node_hash,
	       node_id || '/' AS subtree_lo,
	       node_id || '0' AS subtree_hi
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
func nodesViewBody(qg Querier) (string, error) {
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
		//
		// source_file moved too: v6 leaves nodes.source_file empty, because a
		// node's file is its nid's top bits. On v4 every node under a file
		// names it, and rules scope by it (sleep_in_test, untested_function,
		// dead_code), so the view derives it: the file the nid sits in, and
		// NULL for directories, which sit in none (mache-2dc105).
		return `SELECT n.nid        AS node_id,
		       n.parent_nid AS parent_id,
		       vn.name      AS name,
		       n.kind       AS kind,
		       CASE WHEN n.nid > 0 THEN
		         (SELECT s.id FROM _source s WHERE s.file_id = n.nid >> ` + strconv.Itoa(V6FileShift) + `)
		       END AS source_file
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
