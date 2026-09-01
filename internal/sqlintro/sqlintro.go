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
