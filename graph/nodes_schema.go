package graph

import "database/sql"

// What the nodes table looks like on disk, as distinct from how it is read
// (nodes_table_reader.go) or written (sqlite_writer.go).

// NodeKindFile and NodeKindDir are the integer values stored in the nodes
// table's `kind` column (the wire format produced by mache build /
// leyline parse). They are NOT a substitute for *Node.Mode at runtime —
// Mode uses fs.FileMode and carries the IsDir() predicate. The kind
// constants are used by ingestion writers when populating the column,
// by readers when interpreting it, and as a comment marker next to
// bare-int SQL literals like `kind = 1` (SQL can't reference Go
// constants directly).
//
// These values are part of the on-disk schema and must not change.
//
// Bead mache-e7e9e5.
const (
	NodeKindFile = 0
	NodeKindDir  = 1
)

// ColumnExists reports whether table has a column named col. Readers use it
// to stay compatible with nodes tables written before a column was added
// (e.g. `context`, mache-b8fe72) or produced by leyline, and writers use it
// to decide whether an ALTER is needed.
//
// Deliberately pragma_table_xinfo, NOT pragma_table_info: table_info omits
// GENERATED columns entirely, so it answers "missing" for a column that is
// present, readable, indexed, and returned by SELECT *. ley-line-open already
// ships one (`source_blobs.byte_len` STORED) and projection-v4 makes
// `nodes.parent_id` another (mache-bc6ca3). Under table_info a reader would
// silently downgrade to a compatibility path for a column it can read, and a
// writer would emit an ALTER that fails with "duplicate column name". xinfo
// reports the same rows as info plus generated and hidden ones, so no existing
// probe changes its answer.
func ColumnExists(db *sql.DB, table, col string) bool {
	rows, err := db.Query("SELECT 1 FROM pragma_table_xinfo(?) WHERE name = ?", table, col)
	if err != nil {
		return false
	}
	defer func() { _ = rows.Close() }()
	return rows.Next()
}
