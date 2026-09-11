package ingest

import (
	"database/sql"
	"fmt"
	"os"
	"sync"
)

// fileIndex is one source file's whole section of the ley-line db, held in
// memory: its nodes⋈_ast rows, the node_child lists of those rows' subtrees,
// its _source row and (for Go) its _imports rows. Every question the
// projection asks about a file — which nodes have a kind, who a node's parent
// and siblings are and under which field, the language, the package name, the
// source bytes, the doc comment above a construct, the calls under it, the
// context declarations, the imports — is answered from this section, so a
// file costs the statements that load the section and no more (mache-40ce82:
// four for Go, three otherwise; the gate is
// TestProjectSourceFile_StatementCountIsTheSection).
//
// It began as a node index alone: the per-node SQL finders (findChildByKindAST
// et al.) each scanned ALL same-kind nodes in the file and post-filtered by
// id-path, so resolving one capture per construct made the whole projection
// O(nodes²) — 70% of a whole-repo build's CPU (mache-4f3840). The remaining
// per-file SQL (a query per call pattern, two per construct for doc comments,
// one each for language/package/source/context/imports) was the same pattern
// one level up: ~50 statements per file that all read rows the index already
// held.
type fileIndex struct {
	// all holds every node of the file in start_byte order (the load query's
	// ORDER BY), so first-match and multi-match navigation preserve source
	// order — matching tree-sitter. The other fields index into it.
	all []idxNode
	// byKind groups nodes by _ast.node_kind.
	byKind map[string][]int
	// byID resolves a node id to its position in all.
	byID map[string]int
	// children lists each node's children, start_byte-ordered.
	children map[string][]int
	// comments lists each node's comment children, start_byte-ordered — the
	// doc-comment scan's candidates. Sibling nodes never overlap, so end_byte
	// is monotone along this list and docExtendStart can bisect it.
	comments map[string][]int

	// The _source row, loaded on first need: the serve-path callee extraction
	// reads calls only and never pays for the bytes. A path-mode row reads the
	// file from disk here, once.
	srcOnce sync.Once
	lang    string
	source  []byte
	srcErr  error
}

// idxNode is a materialized nodes⋈_ast row, plus the field it sits under in
// its parent — which is not a column of either table but a property of the
// parent's node_child list (see loadFileIndex). It carries both nodes.kind
// (the int dir/file marker astNode.kind holds) and _ast.node_kind (the
// tree-sitter kind), so toAST reproduces the finders' astNode exactly.
type idxNode struct {
	id        string
	parentID  string
	name      string
	nodeKind  int    // nodes.kind — astNode.kind
	astKind   string // _ast.node_kind
	record    string
	startByte int
	endByte   int
	// hash is _ast.node_hash — the subtree's content address, the key its
	// node_child list is filed under.
	hash string
	// field is the tree-sitter field this node occupies in its parent
	// ("name", "receiver", "type"); "" when it has none, or when the parent
	// is not in the index (the file's root node).
	field string
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
// (idx_ast_source drives the source_id lookup), builds the lookups, then
// reads the node_child lists of the file's subtrees in a second and derives
// each node's field from them.
//
// The field is derived rather than read because ley-line records it on the
// merkle child list, not on the node: node_child lists a parent HASH's
// children in tree-sitter order — every child, anonymous tokens included —
// while _ast holds the named nodes only. So a parent's indexed children, in
// start-byte order, are a subsequence of its list in ordinal order, and one
// forward pass through the list assigns each child the field of the first
// unconsumed row carrying its hash. A child the list does not contain is a
// db that contradicts itself and is an error, not a node without a field.
func (w *ASTWalker) loadFileIndex(sourceID string) (*fileIndex, error) {
	idx := newFileIndex()
	rows, err := w.db.Query(`SELECT n.id, COALESCE(n.parent_id, ''), n.name, n.kind,
	        COALESCE(n.record, ''), a.node_kind, a.start_byte, a.end_byte, a.node_hash
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
		var hash []byte
		if err := rows.Scan(&n.id, &n.parentID, &n.name, &n.nodeKind,
			&n.record, &n.astKind, &n.startByte, &n.endByte, &hash); err != nil {
			// Surface a scan failure rather than silently dropping the row —
			// a partial index would under-populate navigation with no signal
			// (mache-015f5c). The old SQL finders returned this error too.
			return nil, fmt.Errorf("scan file index row for %s: %w", sourceID, err)
		}
		n.hash = string(hash)
		idx.add(n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	lists, err := w.loadChildLists(sourceID)
	if err != nil {
		return nil, err
	}
	if err := idx.assignFields(lists); err != nil {
		return nil, fmt.Errorf("file index for %s: %w", sourceID, err)
	}
	return idx, nil
}

// childListRow is one node_child row: a child subtree's hash and its field.
type childListRow struct {
	hash  string
	field string
}

// loadChildLists reads the node_child lists of every subtree in the file,
// keyed by parent hash, each in ordinal order.
func (w *ASTWalker) loadChildLists(sourceID string) (map[string][]childListRow, error) {
	rows, err := w.db.Query(`SELECT parent_hash, child_hash, COALESCE(field, '')
	 FROM node_child
	 WHERE parent_hash IN (SELECT node_hash FROM _ast WHERE source_id = ?)
	 ORDER BY parent_hash, ordinal`, sourceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	lists := make(map[string][]childListRow)
	for rows.Next() {
		var parent, child []byte
		var field string
		if err := rows.Scan(&parent, &child, &field); err != nil {
			return nil, fmt.Errorf("scan node_child row for %s: %w", sourceID, err)
		}
		lists[string(parent)] = append(lists[string(parent)], childListRow{hash: string(child), field: field})
	}
	return lists, rows.Err()
}

// assignFields sets each node's field from its parent's child list, walking
// the list forward once per parent (see loadFileIndex).
func (idx *fileIndex) assignFields(lists map[string][]childListRow) error {
	for pi := range idx.all {
		parent := &idx.all[pi]
		list := lists[parent.hash]
		ri := 0
		for _, ci := range idx.children[parent.id] {
			child := &idx.all[ci]
			for ri < len(list) && list[ri].hash != child.hash {
				ri++
			}
			if ri == len(list) {
				return fmt.Errorf("node_child list of %s (%x, %d rows) does not contain its child %s (%x)",
					parent.id, parent.hash, len(list), child.id, child.hash)
			}
			child.field = list[ri].field
			ri++
		}
	}
	return nil
}

func newFileIndex() *fileIndex {
	return &fileIndex{
		byKind:   make(map[string][]int),
		byID:     make(map[string]int),
		children: make(map[string][]int),
		comments: make(map[string][]int),
	}
}

// add appends n, which must follow every node added before it in start_byte
// order (the load query's ORDER BY), and indexes it.
func (idx *fileIndex) add(n idxNode) {
	i := len(idx.all)
	idx.all = append(idx.all, n)
	idx.byKind[n.astKind] = append(idx.byKind[n.astKind], i)
	idx.byID[n.id] = i
	idx.children[n.parentID] = append(idx.children[n.parentID], i)
	if n.astKind == "comment" {
		idx.comments[n.parentID] = append(idx.comments[n.parentID], i)
	}
}

// fileSection returns sourceID's index with its _source row loaded. The row's
// own outcome (a missing row, a path-mode row whose file is unreadable) is on
// idx.srcErr, separate from the index load error, because the callers treat
// them differently: no index is a failed projection, no source is a file
// whose constructs project without text — as before.
func (w *ASTWalker) fileSection(sourceID string) (*fileIndex, error) {
	idx, err := w.fileIndex(sourceID)
	if err != nil {
		return nil, err
	}
	idx.srcOnce.Do(func() {
		idx.lang, idx.source, idx.srcErr = readSource(w.db, sourceID)
	})
	return idx, nil
}

// readSource reads one _source row: the language and the content, which is
// either the inline BLOB or — when ley-line stores a path reference instead
// (its default; content is NULL) — the file at that path.
func readSource(db *sql.DB, sourceID string) (lang string, content []byte, err error) {
	var path sql.NullString
	if err := db.QueryRow("SELECT language, content, path FROM _source WHERE id = ?", sourceID).
		Scan(&lang, &content, &path); err != nil {
		return "", nil, err
	}
	if len(content) > 0 {
		return lang, content, nil
	}
	if path.Valid && path.String != "" {
		content, err = os.ReadFile(path.String)
		return lang, content, err
	}
	return lang, nil, fmt.Errorf("_source %s: no content and no path", sourceID)
}
