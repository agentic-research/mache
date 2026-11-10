// Package sqlintro holds SQLite schema introspection shared by packages that
// cannot depend on each other.
//
// It exists because the generated-column probe is needed in three places —
// graph (readers), cmd (the virtual-node writer) and internal/fixturedb (test
// fixtures) — and no two of them can host it for all three. graph's own tests
// import fixturedb, so fixturedb importing graph is a cycle; cmd is a leaf
// nobody imports. Three copies is what the duplicate_code gate objected to, and
// it was right: three implementations of "is this column writable" is three
// chances to answer differently.
package sqlintro

import "database/sql"

// RowQuerier is the single-row query surface shared by *sql.DB and *sql.Tx, so
// one probe serves callers holding either.
type RowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

// ColumnIsGenerated reports whether table.col is a GENERATED column, which is
// readable but rejected at PREPARE time by any INSERT or UPDATE that names it.
// Writers targeting a table they did not create must ask before building a
// column list.
//
// pragma_table_xinfo.hidden encodes the kind: 0 ordinary, 1 hidden
// (virtual-table), 2 GENERATED ... VIRTUAL, 3 GENERATED ... STORED. Only 2 and 3
// are unwritable — a virtual table's hidden column still accepts writes.
// pragma_table_info cannot be substituted: it omits generated columns entirely,
// so it cannot distinguish "derived" from "absent".
//
// A missing table or column reads as not-generated, matching the convention of
// collapsing absence into the negative answer.
func ColumnIsGenerated(q RowQuerier, table, col string) bool {
	var hidden int
	if err := q.QueryRow(
		"SELECT hidden FROM pragma_table_xinfo(?) WHERE name = ?", table, col).Scan(&hidden); err != nil {
		return false
	}
	return hidden == 2 || hidden == 3
}

// ColumnExists reports whether table has a column named col; a missing table
// reads as no column. Readers use it to stay compatible with tables written
// before a column was added (e.g. `context`, mache-b8fe72) or produced by
// leyline, and writers use it to decide whether an ALTER is needed. It takes
// a RowQuerier, so a *sql.DB and a *sql.Tx both work (fixturedb probes the
// table it is writing into, inside its transaction).
//
// Deliberately pragma_table_xinfo, NOT pragma_table_info: table_info omits
// GENERATED columns entirely, so it answers "missing" for a column that is
// present, readable, indexed, and returned by SELECT *. ley-line-open already
// ships one (`source_blobs.byte_len` STORED) and projection-v4 made
// `nodes.parent_id` another (mache-bc6ca3). Under table_info a reader would
// silently downgrade to a compatibility path for a column it can read, and a
// writer would emit an ALTER that fails with "duplicate column name". xinfo
// reports the same rows as info plus generated and hidden ones, so no existing
// probe changes its answer.
func ColumnExists(q RowQuerier, table, col string) bool {
	var one int
	return q.QueryRow("SELECT 1 FROM pragma_table_xinfo(?) WHERE name = ?", table, col).Scan(&one) == nil
}
