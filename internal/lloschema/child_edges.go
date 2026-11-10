package lloschema

// ChildEdges is the LEFT JOIN chain from a node to each of its children that
// sits under a tree-sitter field, and how to read each such child's grammar
// kind on this db. Joins exposes `ce.field` and `ce.ordinal` (the edge); Kind
// is an expression for the child's grammar kind. A node with no fielded child
// keeps its row, with those NULL.
type ChildEdges struct {
	Joins string
	Kind  string
}

// FieldChildJoins builds the ChildEdges for a query that reads the parent
// node's merkle hash as parentHash and its occurrence nid as parentNid.
//
// projection-v9 (leyline v0.22.0) moved the edges into node_edge, keyed on
// node_content.content_id, and left node_child as a view over it. A plain read
// of that view plans as index probes, but SQLite cannot flatten a join view on
// the right of a LEFT JOIN and materializes all of it: LLO measured 7.7 s
// against 0.11 s on one kibana file, and its table contract says to join
// node_edge instead (mache-72668d). Older arenas hold node_child as the table.
//
// projection-v12 (leyline v0.26.0) took raw_kind and lang off node_content:
// they belong to an occurrence, not to the content, since identical subtrees
// in two languages share one node_content row (mache-108e26). A named child's
// grammar kind is its occurrence's, the child row of the parent nid whose
// _ast content is the edge's child, through _ast.kind_id -> kinds; an
// anonymous token has no _ast row, and its node_content.kind is the grammar
// kind verbatim. That is LLO's recipe ("A fielded child's grammar kind at
// projection-v12", TABLE_CONTRACT), which its own test pins against
// tree-sitter. Earlier projections read node_content.raw_kind.
func FieldChildJoins(qg Querier, parentHash, parentNid string) (ChildEdges, error) {
	interned, err := TableHasColumn(qg, "node_edge", "child_id")
	if err != nil {
		return ChildEdges{}, err
	}
	if !interned {
		return ChildEdges{
			Joins: "LEFT JOIN node_child ce ON ce.parent_hash = " + parentHash + ` AND ce.field IS NOT NULL AND ce.field != ''
LEFT JOIN node_content cc ON cc.node_hash = ce.child_hash`,
			Kind: "cc.raw_kind",
		}, nil
	}
	joins := "LEFT JOIN node_content pc ON pc.node_hash = " + parentHash + `
LEFT JOIN node_edge ce ON ce.parent_id = pc.content_id AND ce.field IS NOT NULL AND ce.field != ''
LEFT JOIN node_content cc ON cc.content_id = ce.child_id`
	contentKind, err := TableHasColumn(qg, "node_content", "raw_kind")
	if err != nil {
		return ChildEdges{}, err
	}
	if contentKind {
		return ChildEdges{Joins: joins, Kind: "cc.raw_kind"}, nil
	}
	return ChildEdges{
		Joins: joins,
		Kind: `COALESCE((SELECT k.raw_kind FROM nodes n
		 JOIN _ast ca ON ca.nid = n.nid
		 JOIN kinds k ON k.kind_id = ca.kind_id
		 WHERE n.parent_nid = ` + parentNid + ` AND ca.content_id = ce.child_id
		 ORDER BY n.ord LIMIT 1), cc.kind)`,
	}, nil
}
