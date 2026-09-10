package ingest

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
)

// fileIndex is one source file's whole section of the ley-line db, held in
// memory: its nodes⋈_ast rows, its _source row and (for Go) its _imports rows.
// Every question the projection asks about a file — which nodes have a kind,
// who a node's parent and siblings are, the language, the package name, the
// source bytes, the doc comment above a construct, the calls under it, the
// context declarations, the imports — is answered from this section, so a
// file costs the statements that load the section and no more (mache-40ce82:
// three for Go, two otherwise; the gate is
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

// idxNode is a materialized nodes⋈_ast row. It carries both nodes.kind (the
// int dir/file marker astNode.kind holds) and _ast.node_kind (the tree-sitter
// kind), so toAST reproduces the finders' astNode exactly.
type idxNode struct {
	id        string
	parentID  string
	name      string
	nodeKind  int    // nodes.kind — astNode.kind
	astKind   string // _ast.node_kind
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
// (idx_ast_source drives the source_id lookup) and builds the lookups.
func (w *ASTWalker) loadFileIndex(sourceID string) (*fileIndex, error) {
	idx := newFileIndex()
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
		if err := rows.Scan(&n.id, &n.parentID, &n.name, &n.nodeKind,
			&n.record, &n.astKind, &n.startByte, &n.endByte); err != nil {
			// Surface a scan failure rather than silently dropping the row —
			// a partial index would under-populate navigation with no signal
			// (mache-015f5c). The old SQL finders returned this error too.
			return nil, fmt.Errorf("scan file index row for %s: %w", sourceID, err)
		}
		idx.add(n)
	}
	return idx, rows.Err()
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

// nodesByKind returns the in-memory nodes of a kind whose id lives under
// parentPrefix (empty = whole file). Mirrors findNodesByKind's SQL semantics
// (a.node_kind = kind AND n.id LIKE parentPrefix||'/%').
func (idx *fileIndex) nodesByKind(parentPrefix, kind string) []astNode {
	prefix := parentPrefix + "/"
	var out []astNode
	for _, i := range idx.byKind[kind] {
		n := idx.all[i]
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
	if out := idx.descendantsByKind(parentID, kind, ancestry, 1); len(out) > 0 {
		return &out[0]
	}
	return nil
}

// childrenByKind is the multi-result form of childByKind (all matching
// descendants, ordered by start_byte). Mirrors findChildrenByKindAST.
func (idx *fileIndex) childrenByKind(parentID, kind string, ancestry []string) []astNode {
	return idx.descendantsByKind(parentID, kind, ancestry, 0)
}

// descendantsByKind returns up to limit (0 = all) descendants of parentID of
// the given kind at depth len(ancestry)+1 whose id path matches ancestry.
func (idx *fileIndex) descendantsByKind(parentID, kind string, ancestry []string, limit int) []astNode {
	prefix := parentID + "/"
	depth := len(ancestry) + 1
	var out []astNode
	for _, i := range idx.byKind[kind] {
		n := idx.all[i]
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
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out
}

// segmentCount counts '/'-separated path segments in a node-id suffix.
func segmentCount(suffix string) int {
	return strings.Count(suffix, "/") + 1
}
