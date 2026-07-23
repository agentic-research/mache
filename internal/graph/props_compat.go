package graph

import (
	"database/sql"
	"errors"
	"fmt"
)

// ErrStalePropsSchema is returned for a nodes table mache itself wrote before
// the props column existed. Exposed so callers can distinguish "rebuild this"
// from a genuine open failure.
var ErrStalePropsSchema = errors.New("nodes table predates the props column")

// RequireProps rejects a nodes table that mache wrote before the props column
// existed (mache-90b89b). Such a table has Properties base64'd into `record`,
// a format no current code reads — serving it would silently yield constructs
// with no lang/pkg/imports rather than failing.
//
// The check is scoped by producer. mache's SQLiteWriter always has `context`
// (it is in CREATE TABLE, and ALTERed in for older tables); leyline-produced
// tables stop at source_file and carry no Properties at all. So `context`
// present with `props` absent is exactly "stale mache-written", and is the only
// case refused — a blanket refusal would reject every leyline .db, which since
// v0.18.0 is mache's primary input.
func RequireProps(db *sql.DB) error {
	if !ColumnExists(db, "nodes", "context") {
		return nil // leyline-produced: never carried Properties, nothing to lose
	}
	if ColumnExists(db, "nodes", "props") {
		return nil
	}
	return fmt.Errorf(
		"%w and its node properties are unreadable; rebuild it with `mache build`",
		ErrStalePropsSchema)
}
