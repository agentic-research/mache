package lloschema

// FieldChildJoins is the LEFT JOIN chain from a node to each of its children
// that sits under a tree-sitter field, for a query that reads the node's
// merkle hash as parentHash. It exposes `ce.field` and `ce.ordinal` (the edge)
// and `cc.raw_kind` (the child's content); a node with no fielded child keeps
// its row, with those NULL.
//
// projection-v9 (leyline v0.22.0) moved the edges into node_edge, keyed on
// node_content.content_id, and left node_child as a view over it. A plain read
// of that view plans as index probes, but SQLite cannot flatten a join view on
// the right of a LEFT JOIN and materializes all of it: LLO measured 7.7 s
// against 0.11 s on one kibana file, and its table contract says to join
// node_edge instead (mache-72668d). Older arenas hold node_child as the table,
// so the chain is chosen per db.
func FieldChildJoins(qg Querier, parentHash string) (string, error) {
	interned, err := TableHasColumn(qg, "node_edge", "child_id")
	if err != nil {
		return "", err
	}
	if interned {
		return "LEFT JOIN node_content pc ON pc.node_hash = " + parentHash + `
LEFT JOIN node_edge ce ON ce.parent_id = pc.content_id AND ce.field IS NOT NULL AND ce.field != ''
LEFT JOIN node_content cc ON cc.content_id = ce.child_id`, nil
	}
	return "LEFT JOIN node_child ce ON ce.parent_hash = " + parentHash + ` AND ce.field IS NOT NULL AND ce.field != ''
LEFT JOIN node_content cc ON cc.node_hash = ce.child_hash`, nil
}
