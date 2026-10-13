package ingest

import (
	"database/sql"
	"fmt"
	"path"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/lloschema"
)

// ley-line-open's projection-v6 (v0.20.x) keys _ast and nodes by integer nid
// and stores no paths (mache-90cb9f). The walker's in-memory file index is
// keyed by path, because scoping and call attribution work by id prefix and
// the ids it hands out become the projection's node ids. So on v6 the file's
// section is loaded by nid, and each node's path is built from its parent's
// path plus its name from LLO's own v_node_name, the one owner of the naming
// rule. Everything after the load is the same for both shapes.

// isV6 reports whether the db is projection-v6, asking once. A failed probe is
// not remembered, as with hasImportsTable.
func (w *ASTWalker) isV6() bool {
	w.shapeMu.Lock()
	defer w.shapeMu.Unlock()
	if !w.shapeProbed {
		w.v6, w.shapeProbed = graph.ColumnExists(w.db, "_ast", "nid"), true
	}
	return w.v6
}

// v6Row is one node of a file as the v6 section query returns it.
type v6Row struct {
	nid, parent int64
	n           idxNode
	located     bool // has an _ast row
}

// loadFileIndexV6 reads every node of one file in a single query, by nid
// range, and builds the same index loadFileIndex builds from a v4 file.
//
// Nodes are read in nid order. LLO numbers a file's nodes in preorder by
// sibling position, so that order is start_byte order with a parent ahead of
// a first child that starts where it does: the order the index needs, and the
// order v4's ORDER BY start_byte gives. It also means a parent's path is known
// before its children are reached. Every node is read, not only those with an
// _ast row, so the chain of names is never broken; only located nodes enter
// the index, as the v4 join keeps only those.
func (w *ASTWalker) loadFileIndexV6(sourceID string) (*fileIndex, error) {
	rows, err := w.db.Query(`SELECT n.nid, COALESCE(n.parent_nid, 0), v.name, n.kind,
	        COALESCE(n.record, ''), COALESCE(k.raw_kind, ''), a.nid IS NOT NULL,
	        COALESCE(a.start_byte, 0), COALESCE(a.end_byte, 0), a.node_hash
	 FROM nodes n
	 JOIN v_node_name v ON v.nid = n.nid
	 LEFT JOIN _ast a ON a.nid = n.nid
	 LEFT JOIN kinds k ON k.kind_id = a.kind_id
	 WHERE `+lloschema.V6FileNids("n.nid")+`
	 ORDER BY n.nid`, sourceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	paths := map[int64]string{}
	idx := newFileIndex()
	for rows.Next() {
		var r v6Row
		var hash []byte
		if err := rows.Scan(&r.nid, &r.parent, &r.n.name, &r.n.nodeKind, &r.n.record,
			&r.n.astKind, &r.located, &r.n.startByte, &r.n.endByte, &hash); err != nil {
			return nil, fmt.Errorf("scan file index row for %s: %w", sourceID, err)
		}
		r.n.hash = string(hash)
		if r.nid&(1<<lloschema.V6FileShift-1) == 0 {
			// The file node (ordinal 0): its path is the _source id, its
			// parent the directory above it ("" at the root, as v4's
			// COALESCE gives).
			r.n.id = sourceID
			if dir := path.Dir(sourceID); dir != "." {
				r.n.parentID = dir
			}
		} else {
			parent, ok := paths[r.parent]
			if !ok {
				return nil, fmt.Errorf("file index for %s: node %d's parent %d is not in the file", sourceID, r.nid, r.parent)
			}
			r.n.id, r.n.parentID = parent+"/"+r.n.name, parent
		}
		paths[r.nid] = r.n.id
		if r.located {
			idx.add(r.n)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	lists, err := w.queryChildLists(`SELECT parent_hash, child_hash, COALESCE(field, '')
	 FROM node_child
	 WHERE parent_hash IN (SELECT node_hash FROM _ast WHERE `+lloschema.V6FileNids("nid")+`)
	 ORDER BY parent_hash, ordinal`, sourceID)
	if err != nil {
		return nil, err
	}
	if err := idx.assignFields(lists); err != nil {
		return nil, fmt.Errorf("file index for %s: %w", sourceID, err)
	}
	return idx, nil
}

// sqlDB lets a *sql.DB answer lloschema's Querier.
type sqlDB struct{ *sql.DB }

func (d sqlDB) QueryRefs(query string, args ...any) (*sql.Rows, error) {
	return d.Query(query, args...)
}
