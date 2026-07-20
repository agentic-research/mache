package ingest

import (
	"database/sql"
	"strings"
)

// findNodesByKind finds all nodes of a specific kind under a parent prefix.
// Ley-line disambiguates siblings of the same kind with suffixes (e.g.,
// function_declaration_0, function_declaration_1). We match by _ast.node_kind
// which stores the original tree-sitter kind without suffixes.
func (w *ASTWalker) findNodesByKind(db *sql.DB, parentPrefix, kind, sourceID string) ([]astNode, error) {
	// Per-file fast path: answer from the in-memory index (loaded once) instead
	// of a fresh indexed scan per call (mache-4f3840). The whole-db path
	// (sourceID == "") has no single file to materialize, so it keeps the SQL.
	if sourceID != "" {
		idx, err := w.fileIndex(sourceID)
		if err != nil {
			return nil, err
		}
		return idx.nodesByKind(parentPrefix, kind), nil
	}
	query := `SELECT n.id, n.parent_id, n.name, n.kind, COALESCE(n.record, ''),
	                COALESCE(a.start_byte, 0), COALESCE(a.end_byte, 0)
	         FROM nodes n
	         JOIN _ast a ON a.node_id = n.id
	         WHERE a.node_kind = ?`
	args := []any{kind}

	if parentPrefix != "" {
		query += " AND n.id LIKE ?"
		args = append(args, parentPrefix+"/%")
	}
	if sourceID != "" {
		query += " AND a.source_id = ?"
		args = append(args, sourceID)
	}

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var nodes []astNode
	for rows.Next() {
		var n astNode
		if err := rows.Scan(&n.id, &n.parentID, &n.name, &n.kind, &n.record,
			&n.startByte, &n.endByte); err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

// findChildrenByKindAST finds ALL descendants matching a node_kind under a
// parent, at the depth implied by ancestry (empty ancestry = direct children),
// ordered by start_byte. It is the multi-result sibling of findChildByKindAST:
// used when a selector's @scope is an INNER node kind that can occur multiple
// times under one outer match — e.g. grouped Go declarations `type ( A; B )`,
// where a single type_declaration contains two type_spec nodes. tree-sitter
// yields one match per inner node; this mirrors that.
func (w *ASTWalker) findChildrenByKindAST(db *sql.DB, parentID, kind, sourceID string, ancestry []string) ([]astNode, error) {
	// In-memory fast path (mache-4f3840); whole-db path keeps the SQL below.
	if sourceID != "" {
		idx, err := w.fileIndex(sourceID)
		if err != nil {
			return nil, err
		}
		return idx.childrenByKind(parentID, kind, ancestry), nil
	}
	const baseCols = `SELECT n.id, n.parent_id, n.name, n.kind, COALESCE(n.record, ''),
	        COALESCE(a.start_byte, 0), COALESCE(a.end_byte, 0)
	 FROM nodes n
	 JOIN _ast a ON a.node_id = n.id
	 WHERE `

	var query string
	var args []any
	if len(ancestry) == 0 {
		query = baseCols + "n.id LIKE ? AND n.id NOT LIKE ? AND a.node_kind = ?"
		args = []any{parentID + "/%", parentID + "/%/%", kind}
	} else {
		var depthPattern strings.Builder
		depthPattern.WriteString(parentID)
		for range len(ancestry) + 1 {
			depthPattern.WriteString("/%")
		}
		query = baseCols + "n.id LIKE ? AND n.id NOT LIKE ? AND a.node_kind = ?"
		args = []any{depthPattern.String(), depthPattern.String() + "/%", kind}
	}
	if sourceID != "" {
		query += " AND a.source_id = ?"
		args = append(args, sourceID)
	}
	query += " ORDER BY a.start_byte ASC"

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []astNode
	for rows.Next() {
		var n astNode
		if err := rows.Scan(&n.id, &n.parentID, &n.name, &n.kind, &n.record, &n.startByte, &n.endByte); err != nil {
			continue
		}
		if len(ancestry) > 0 {
			suffix := strings.TrimPrefix(n.id, parentID+"/")
			if !matchAncestry(suffix, ancestry) {
				continue
			}
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// findChildByKindAST finds the first descendant matching a node_kind via _ast table,
// optionally verifying that the node's ID path contains the required ancestor kinds.
// Ordered by start_byte ASC for deterministic first-occurrence behavior.
//
// When ancestry is non-empty, the query constrains depth in SQL using a
// LIKE pattern with exactly len(ancestry)+1 path segments (ancestors + leaf),
// plus a NOT LIKE excluding deeper descendants. This avoids scanning all
// descendants in Go — only nodes at the exact expected depth are returned.
func (w *ASTWalker) findChildByKindAST(db *sql.DB, parentID, kind, sourceID string, ancestry []string) (*astNode, error) {
	// In-memory fast path (mache-4f3840); whole-db path keeps the SQL below.
	if sourceID != "" {
		idx, err := w.fileIndex(sourceID)
		if err != nil {
			return nil, err
		}
		return idx.childByKind(parentID, kind, ancestry), nil
	}
	const baseCols = `SELECT n.id, n.parent_id, n.name, n.kind, COALESCE(n.record, ''),
	        COALESCE(a.start_byte, 0), COALESCE(a.end_byte, 0)
	 FROM nodes n
	 JOIN _ast a ON a.node_id = n.id
	 WHERE `

	var query string
	var args []any

	if len(ancestry) == 0 {
		// No ancestry — direct child only (depth 1 below parent).
		// Tree-sitter S-expressions like `(parent (child) @cap)` mean
		// child is a direct child; we mirror that by constraining depth.
		query = baseCols + "n.id LIKE ? AND n.id NOT LIKE ? AND a.node_kind = ?"
		args = []any{parentID + "/%", parentID + "/%/%", kind}
	} else {
		// Ancestry constraint — restrict to exact depth.
		// For ancestry=["parameter_list","parameter_declaration"], build:
		//   LIKE 'parentID/%/%/%'         (3 segments: 2 ancestors + leaf)
		//   NOT LIKE 'parentID/%/%/%/%'   (exclude deeper nodes)
		var depthPattern strings.Builder
		depthPattern.WriteString(parentID)
		for range len(ancestry) + 1 {
			depthPattern.WriteString("/%")
		}
		query = baseCols + "n.id LIKE ? AND n.id NOT LIKE ? AND a.node_kind = ?"
		args = []any{depthPattern.String(), depthPattern.String() + "/%", kind}
	}

	if sourceID != "" {
		query += " AND a.source_id = ?"
		args = append(args, sourceID)
	}
	query += " ORDER BY a.start_byte ASC"

	if len(ancestry) == 0 {
		// No ancestry — first match wins.
		query += " LIMIT 1"
		var n astNode
		err := db.QueryRow(query, args...).Scan(&n.id, &n.parentID, &n.name, &n.kind, &n.record, &n.startByte, &n.endByte)
		if err == sql.ErrNoRows {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return &n, nil
	}

	// With ancestry: depth is constrained in SQL, but we still verify
	// the exact kind sequence in Go (LIKE % wildcards don't check kinds).
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var n astNode
		if err := rows.Scan(&n.id, &n.parentID, &n.name, &n.kind, &n.record, &n.startByte, &n.endByte); err != nil {
			continue
		}
		suffix := strings.TrimPrefix(n.id, parentID+"/")
		if matchAncestry(suffix, ancestry) {
			return &n, nil
		}
	}
	return nil, nil
}
