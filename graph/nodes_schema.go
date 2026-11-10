package graph

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
