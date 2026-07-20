package ingest

import "strings"

// fileIndex materializes one source file's nodes/_ast rows in memory so the
// ASTWalker can navigate the tree without a SQL query per parent node. The
// per-node SQL finders (findChildByKindAST et al.) each scanned ALL same-kind
// nodes in the file and post-filtered by id-path, so resolving one capture per
// construct made the whole projection O(nodes²) — 70% of a whole-repo build's
// CPU (mache-4f3840). Loading the file once (a single indexed query) and
// bucketing by node_kind turns every lookup into an in-memory slice filter,
// restoring the O(nodes) tree walk SitterWalker did over its parsed tree.
type fileIndex struct {
	// byKind groups nodes by their _ast.node_kind, each slice ordered by
	// start_byte ASC (the load query's ORDER BY) so first-match and
	// multi-match navigation preserve source order — matching tree-sitter.
	byKind map[string][]idxNode
}

// idxNode is a materialized nodes⋈_ast row. It carries both nodes.kind (the
// int dir/file marker astNode.kind holds) and _ast.node_kind (the tree-sitter
// kind used for grouping), so toAST reproduces the finders' astNode exactly.
type idxNode struct {
	id        string
	parentID  string
	name      string
	nodeKind  int // nodes.kind — astNode.kind
	record    string
	startByte int
	endByte   int
}

func (n idxNode) toAST() astNode {
	return astNode{
		id:        n.id,
		parentID:  n.parentID,
		name:      n.name,
		kind:      n.nodeKind,
		record:    n.record,
		startByte: n.startByte,
		endByte:   n.endByte,
	}
}

// fileIndex returns the cached in-memory index for sourceID, loading it once.
// A load failure (e.g. a closed/broken DB) is surfaced, not swallowed: the
// callers wrap it so the projection fails loudly rather than silently
// producing empty results. A failed load is not cached.
func (w *ASTWalker) fileIndex(sourceID string) (*fileIndex, error) {
	if v, ok := w.indexCache.Load(sourceID); ok {
		return v.(*fileIndex), nil
	}
	idx, err := w.loadFileIndex(sourceID)
	if err != nil {
		return nil, err
	}
	w.indexCache.Store(sourceID, idx)
	return idx, nil
}

// loadFileIndex reads every node of one file in a single indexed query
// (idx_ast_source drives the source_id lookup) and buckets by node_kind.
func (w *ASTWalker) loadFileIndex(sourceID string) (*fileIndex, error) {
	idx := &fileIndex{byKind: make(map[string][]idxNode)}
	rows, err := w.db.Query(`SELECT n.id, COALESCE(n.parent_id, ''), n.name, n.kind,
	        COALESCE(n.record, ''), a.node_kind, a.start_byte, a.end_byte
	 FROM nodes n
	 JOIN _ast a ON a.node_id = n.id
	 WHERE a.source_id = ?
	 ORDER BY a.start_byte ASC`, sourceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var n idxNode
		var astKind string
		if err := rows.Scan(&n.id, &n.parentID, &n.name, &n.nodeKind,
			&n.record, &astKind, &n.startByte, &n.endByte); err != nil {
			continue
		}
		idx.byKind[astKind] = append(idx.byKind[astKind], n)
	}
	return idx, rows.Err()
}

// nodesByKind returns the in-memory nodes of a kind whose id lives under
// parentPrefix (empty = whole file). Mirrors findNodesByKind's SQL semantics
// (a.node_kind = kind AND n.id LIKE parentPrefix||'/%').
func (idx *fileIndex) nodesByKind(parentPrefix, kind string) []astNode {
	nodes := idx.byKind[kind]
	if len(nodes) == 0 {
		return nil
	}
	prefix := parentPrefix + "/"
	var out []astNode
	for _, n := range nodes {
		if parentPrefix != "" && !strings.HasPrefix(n.id, prefix) {
			continue
		}
		out = append(out, n.toAST())
	}
	return out
}

// childByKind returns the first (by start_byte) descendant of parentID with the
// given kind at the depth implied by ancestry, or nil. Mirrors
// findChildByKindAST: direct child when ancestry is empty (depth 1), else
// exactly len(ancestry)+1 path segments below parentID with the kind sequence
// verified by matchAncestry.
func (idx *fileIndex) childByKind(parentID, kind string, ancestry []string) *astNode {
	prefix := parentID + "/"
	depth := len(ancestry) + 1
	for _, n := range idx.byKind[kind] {
		if !strings.HasPrefix(n.id, prefix) {
			continue
		}
		suffix := n.id[len(prefix):]
		if segmentCount(suffix) != depth {
			continue
		}
		if len(ancestry) > 0 && !matchAncestry(suffix, ancestry) {
			continue
		}
		node := n.toAST()
		return &node
	}
	return nil
}

// childrenByKind is the multi-result form of childByKind (all matching
// descendants, ordered by start_byte). Mirrors findChildrenByKindAST.
func (idx *fileIndex) childrenByKind(parentID, kind string, ancestry []string) []astNode {
	prefix := parentID + "/"
	depth := len(ancestry) + 1
	var out []astNode
	for _, n := range idx.byKind[kind] {
		if !strings.HasPrefix(n.id, prefix) {
			continue
		}
		suffix := n.id[len(prefix):]
		if segmentCount(suffix) != depth {
			continue
		}
		if len(ancestry) > 0 && !matchAncestry(suffix, ancestry) {
			continue
		}
		out = append(out, n.toAST())
	}
	return out
}

// segmentCount counts '/'-separated path segments in a node-id suffix.
func segmentCount(suffix string) int {
	return strings.Count(suffix, "/") + 1
}
