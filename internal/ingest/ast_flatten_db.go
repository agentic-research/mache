package ingest

import (
	"database/sql"
	"fmt"
)

// FlattenASTDB streams FCA records from a leyline-produced `_ast` database,
// mirroring the record shape FlattenAST builds from an in-process tree-sitter
// parse: one record per `_ast` row (leyline stores only named nodes — the
// same population FlattenAST walks) with
//
//	{"type": node_kind, "has_<field>": true, "field_<field>_type": <child raw_kind>}
//
// for every node_child entry carrying a tree-sitter field name. node_child
// includes anonymous children too (e.g. the `func` keyword, the `+` operator);
// fieldless ones carry a NULL field and are skipped, while anonymous children
// WITH a field (e.g. binary_expression's `operator`) are recorded with their
// raw_kind — exactly matching FlattenAST, whose child.Type() for an anonymous
// token is the token itself.
//
// The parent_hash→child join is content-level: identical subtrees share a
// node_hash, so each `_ast` occurrence of a duplicated subtree re-joins the
// same children and produces its own record — matching FlattenAST emitting
// one record per occurrence.
//
// sourceLike filters `_ast.source_id` with SQL LIKE (empty = all sources);
// limit bounds the number of RECORDS returned (0 = unlimited). Ordering is
// deterministic document order (source, then position, pre-order on ties).
//
// Caveat: language-specific enrichment (lang.Language.EnrichNode — currently
// only HCL/Terraform's enrichHCLNode, which is sitter-typed) is NOT
// replicated here. HCL inference via `_ast` therefore lacks that enrichment
// until it is ported; Go and every other language are unaffected.
func FlattenASTDB(db *sql.DB, sourceLike string, limit int) ([]any, error) {
	// LEFT JOIN keeps field-less nodes (they still yield a bare {"type": ...}
	// record). The field filter lives in the JOIN condition, not WHERE, so it
	// prunes fieldless child rows without dropping the parent node. `field`
	// is NULL for fieldless children in leyline's schema; the != '' guard is
	// defensive belt-and-suspenders.
	//
	// ORDER BY reproduces FlattenAST's pre-order document traversal:
	// start_byte ascending, end_byte descending (a parent sharing its first
	// child's start byte spans further, so it sorts first), node_id as the
	// tiebreak for identical spans (node_ids are materialized paths, so a
	// parent's id is a strict prefix of its child's and sorts first), and
	// finally ordinal so duplicate field names resolve last-wins like
	// FlattenAST's child loop.
	const q = `
SELECT a.node_id, a.node_kind, COALESCE(nc.field, ''), COALESCE(cc.raw_kind, '')
FROM _ast a
LEFT JOIN node_child nc ON nc.parent_hash = a.node_hash AND nc.field IS NOT NULL AND nc.field != ''
LEFT JOIN node_content cc ON cc.node_hash = nc.child_hash
WHERE (? = '' OR a.source_id LIKE ?)
ORDER BY a.source_id, a.start_byte, a.end_byte DESC, a.node_id, nc.ordinal`

	rows, err := db.Query(q, sourceLike, sourceLike)
	if err != nil {
		return nil, fmt.Errorf("flatten _ast: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var records []any
	var cur map[string]any
	var curID string

	for rows.Next() {
		var nodeID, nodeKind, field, childKind string
		if err := rows.Scan(&nodeID, &nodeKind, &field, &childKind); err != nil {
			return nil, fmt.Errorf("flatten _ast scan: %w", err)
		}

		if cur == nil || nodeID != curID {
			if cur != nil {
				records = append(records, cur)
				if limit > 0 && len(records) >= limit {
					return records, rows.Err()
				}
			}
			cur = map[string]any{"type": nodeKind}
			curID = nodeID
		}

		if field != "" {
			cur["has_"+field] = true
			cur["field_"+field+"_type"] = childKind
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("flatten _ast rows: %w", err)
	}
	if cur != nil {
		records = append(records, cur)
	}
	return records, nil
}
