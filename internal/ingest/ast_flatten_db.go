package ingest

import (
	"database/sql"
	"fmt"

	"github.com/agentic-research/mache/internal/lloschema"
)

// FlattenASTDB streams FCA records from a leyline-produced `_ast` database,
// mirroring the record shape FlattenAST builds from an in-process tree-sitter
// parse: one record per `_ast` row (leyline stores only named nodes — the
// same population FlattenAST walks) with
//
//	{"type": node_kind, "has_<field>": true, "field_<field>_type": <child raw_kind>}
//
// for every child edge carrying a tree-sitter field name. The edges
// include anonymous children too (e.g. the `func` keyword, the `+` operator);
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
// Comments are not records. From projection-v13 they are `_ast` occurrences,
// and mache folds them rather than projecting them as nodes (mache-1e3d11),
// so a schema inferred from the records must not grow a comment concept.
//
// Caveat: language-specific enrichment (lang.Language.EnrichNode — currently
// only HCL/Terraform's enrichHCLNode, which is sitter-typed) is NOT
// replicated here. HCL inference via `_ast` therefore lacks that enrichment
// until it is ported; Go and every other language are unaffected.
func FlattenASTDB(db *sql.DB, sourceLike string, limit int) ([]any, error) {
	// The child joins (lloschema.FieldChildJoins) are LEFT JOINs, so
	// field-less nodes still yield a bare {"type": ...} record; the field
	// filter lives in the join condition, pruning fieldless children without
	// dropping the parent.
	//
	// ORDER BY reproduces FlattenAST's pre-order document traversal:
	// start_byte ascending, end_byte descending (a parent sharing its first
	// child's start byte spans further, so it sorts first), node_id as the
	// tiebreak for identical spans (node_ids are materialized paths, so a
	// parent's id is a strict prefix of its child's and sorts first), and
	// finally ordinal so duplicate field names resolve last-wins like
	// FlattenAST's child loop.
	//
	// The rows come through lloschema's ASTSelect, the body of v_ast, so the
	// same query reads projection-v4 and projection-v6 (mache-90cb9f). On v6
	// node_id is the nid, which is preorder, so the parent-first tiebreak
	// holds there too.
	body, err := lloschema.ASTSelect(lloschema.DB(db))
	if err != nil {
		return nil, fmt.Errorf("flatten _ast: %w", err)
	}
	if body == "" {
		return nil, fmt.Errorf("flatten _ast: the database has no _ast table mache can read")
	}
	// a.node_id is the occurrence: the nid on projection-v6 and later, which
	// is what the v12 child-kind lookup needs.
	children, err := lloschema.FieldChildJoins(lloschema.DB(db), "a.node_hash", "a.node_id")
	if err != nil {
		return nil, fmt.Errorf("flatten _ast: %w", err)
	}
	q := `
SELECT a.node_id, a.node_kind, COALESCE(ce.field, ''), COALESCE(` + children.Kind + `, '')
FROM (` + body + `) a
` + children.Joins + `
WHERE (? = '' OR a.source_id LIKE ?) AND NOT a.is_comment
ORDER BY a.source_id, a.start_byte, a.end_byte DESC, a.node_id, ce.ordinal`

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
