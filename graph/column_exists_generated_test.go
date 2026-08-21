package graph

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// openGeneratedColumnDB builds a table carrying one ordinary column, one
// GENERATED VIRTUAL column and one GENERATED STORED column. Both generated
// kinds appear because pragma_table_xinfo distinguishes them (hidden 2 vs 3)
// and pragma_table_info omits BOTH — a probe that handled only one kind would
// still be wrong for ley-line-open, which ships one of each:
// nodes.parent_id VIRTUAL (projection-v4) and source_blobs.byte_len STORED.
func openGeneratedColumnDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.Exec(`CREATE TABLE nodes (
		id TEXT PRIMARY KEY,
		parent_id TEXT GENERATED ALWAYS AS (
			CASE WHEN length(id) > length(name)
			     THEN substr(id, 1, length(id) - length(name) - 1)
			     ELSE '' END
		) VIRTUAL,
		name TEXT NOT NULL,
		byte_len INTEGER GENERATED ALWAYS AS (length(name)) STORED
	)`)
	require.NoError(t, err)
	return db
}

// TestColumnExists_SeesGeneratedColumns is the regression that ley-line-open's
// projection-v4 forced (mache-bc6ca3).
//
// ColumnExists gates compatibility paths: readers use it to decide whether a
// column is available, and internal/ingest uses it to decide whether an ALTER
// is needed. Implemented over pragma_table_info it answers "missing" for a
// column that is present, readable, indexed and returned by SELECT * — so a
// reader silently downgrades and a writer emits an ALTER that fails with
// "duplicate column name". The failure is invisible in the reader's case,
// which is what makes it worth a test rather than a comment.
//
// The second half runs the OLD instrument on the same table, so the test
// documents the exact difference it exists to preserve rather than merely
// asserting the new behaviour.
func TestColumnExists_SeesGeneratedColumns(t *testing.T) {
	db := openGeneratedColumnDB(t)

	for _, col := range []string{"id", "name", "parent_id", "byte_len"} {
		assert.True(t, ColumnExists(db, "nodes", col),
			"%s is selectable, so ColumnExists must report it present", col)
	}

	// The instrument this replaced, on the same table.
	tableInfoSees := func(col string) bool {
		var n int
		require.NoError(t, db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('nodes') WHERE name = ?`, col).Scan(&n))
		return n > 0
	}
	assert.True(t, tableInfoSees("id"), "sanity: table_info sees ordinary columns")
	assert.False(t, tableInfoSees("parent_id"),
		"table_info omits GENERATED VIRTUAL columns — the bug this fixes")
	assert.False(t, tableInfoSees("byte_len"),
		"table_info omits GENERATED STORED columns too")
}

// TestColumnExists_StillAnswersFalseForAbsent guards the other direction: the
// move to xinfo must not turn ColumnExists into a function that says yes to
// everything, which is how a probe silently stops gating anything.
func TestColumnExists_StillAnswersFalseForAbsent(t *testing.T) {
	db := openGeneratedColumnDB(t)

	assert.False(t, ColumnExists(db, "nodes", "no_such_column"))
	assert.False(t, ColumnExists(db, "no_such_table", "id"),
		"a missing table collapses to false rather than erroring")
}

// TestColumnIsGenerated_ClassifiesByWritability pins the distinction that
// decides whether a writer may name a column: generated columns are readable
// but rejected at PREPARE time by any INSERT or UPDATE naming them.
func TestColumnIsGenerated_ClassifiesByWritability(t *testing.T) {
	db := openGeneratedColumnDB(t)

	assert.False(t, ColumnIsGenerated(db, "nodes", "id"), "ordinary column is writable")
	assert.False(t, ColumnIsGenerated(db, "nodes", "name"), "ordinary column is writable")
	assert.True(t, ColumnIsGenerated(db, "nodes", "parent_id"), "GENERATED ... VIRTUAL")
	assert.True(t, ColumnIsGenerated(db, "nodes", "byte_len"), "GENERATED ... STORED")

	assert.False(t, ColumnIsGenerated(db, "nodes", "no_such_column"),
		"absence is not generatedness")
	assert.False(t, ColumnIsGenerated(db, "no_such_table", "id"))

	// The classification is only useful if it predicts the write failure, so
	// assert the behaviour it stands for rather than trusting the pragma.
	_, err := db.Exec(`INSERT INTO nodes (id, name) VALUES ('a/b', 'b')`)
	require.NoError(t, err, "an INSERT omitting the generated columns must succeed")

	_, err = db.Exec(`INSERT INTO nodes (id, parent_id, name) VALUES ('x/y', 'x', 'y')`)
	require.Error(t, err, "naming a generated column must fail")
	assert.Contains(t, err.Error(), "cannot INSERT into generated column")

	_, err = db.Exec(`UPDATE nodes SET parent_id = 'z' WHERE id = 'a/b'`)
	require.Error(t, err, "updating a generated column must fail")

	// And the derived value is the one a stored column would have held.
	var parent string
	require.NoError(t, db.QueryRow(
		`SELECT parent_id FROM nodes WHERE id = 'a/b'`).Scan(&parent))
	assert.Equal(t, "a", parent)
}
