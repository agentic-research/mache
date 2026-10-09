package graph

import "database/sql"

// nodeScan is one `nodes` row plus the `_ast` columns joined onto it. It exists
// so GetNode reads as "query, then build the node" rather than fifty lines of
// conditional column assembly — the smell gate flagged the inlined version as
// a long function, and it was right.
type nodeScan struct {
	kind, size int
	mtimeNano  int64
	recordID   sql.NullString
	context    []byte
	props      []byte
	loc        astLocation
}

// nodeSelect builds the statement, and scanTargets builds the destinations, in
// the SAME order. They are adjacent and must stay that way: a column added to
// one without the other scans a value into the wrong field, which SQLite will
// not necessarily reject if the types happen to be compatible.
func (r *NodesTableReader) nodeSelect() string {
	cols := "n.kind, n.size, n.mtime, n.record_id"
	if r.hasProps {
		cols += ", n.props"
	}
	if r.hasContext {
		cols += ", n.context"
	}
	from := "nodes n"
	// The source location rides along on THIS query rather than a second one.
	// GetNode is called per-node inside bulk walks — get_architecture BFSes up
	// to 50,000 nodes (cmd/serve_architecture.go) reading only Mode.IsDir() —
	// so a separate SELECT against _ast would add one round-trip per node to
	// callers that never read Origin. A LEFT JOIN costs nothing extra when the
	// row is absent, and nothing at all when the db has no _ast (mache-e57065).
	if r.hasAST {
		cols += ", a.source_id, a.start_byte, a.end_byte, a.start_row, a.start_col, a.end_row, a.end_col"
		from += " LEFT JOIN _ast a ON a.node_id = n.id"
	}
	return "SELECT " + cols + " FROM " + from + " WHERE n.id = ?"
}

func (row *nodeScan) scanTargets(r *NodesTableReader) []any {
	dest := []any{&row.kind, &row.size, &row.mtimeNano, &row.recordID}
	if r.hasProps {
		dest = append(dest, &row.props)
	}
	if r.hasContext {
		dest = append(dest, &row.context)
	}
	if r.hasAST {
		dest = append(dest, &row.loc.sourceID, &row.loc.startByte, &row.loc.endByte,
			&row.loc.startRow, &row.loc.startCol, &row.loc.endRow, &row.loc.endCol)
	}
	return dest
}

// astLocation holds the nullable `_ast` columns a GetNode LEFT JOIN produces.
// Every field is nullable because the join misses for directories and virtual
// nodes, which have no parse-tree row.
type astLocation struct {
	sourceID           sql.NullString
	startByte, endByte sql.NullInt64
	startRow, startCol sql.NullInt64
	endRow, endCol     sql.NullInt64
}

// origin converts the joined row into a SourceOrigin, or nil when the node has
// no `_ast` row — the documented "not locatable" sentinel. A zero-valued
// Origin would read as "line 0 of an empty file" to every consumer, which is
// worse than admitting we do not know.
//
// Rows and columns are tree-sitter's 0-based; they are stored 1-based here so
// a consumer can use them directly and so 0 unambiguously means unknown. Byte
// offsets pass through UNCHANGED — write-back splices by byte and must not get
// the +1 the reader-facing units need.
func (l astLocation) origin() *SourceOrigin {
	if !l.sourceID.Valid {
		return nil
	}
	return &SourceOrigin{
		FilePath:  l.sourceID.String,
		StartByte: uint32(l.startByte.Int64),
		EndByte:   uint32(l.endByte.Int64),
		StartLine: uint32(l.startRow.Int64) + 1,
		StartCol:  uint32(l.startCol.Int64) + 1,
		EndLine:   uint32(l.endRow.Int64) + 1,
		EndCol:    uint32(l.endCol.Int64) + 1,
	}
}
