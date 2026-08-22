package sqlintro

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// openMixedColumnDB builds a table with one ordinary column, one GENERATED
// VIRTUAL and one GENERATED STORED. Both generated kinds appear because
// pragma_table_xinfo distinguishes them (hidden 2 vs 3) while table_info omits
// BOTH — and ley-line-open ships one of each: nodes.parent_id VIRTUAL
// (projection-v4) and source_blobs.byte_len STORED.
func openMixedColumnDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	// ONE connection, or this fixture is a coin flip: every pooled connection
	// to ":memory:" gets its OWN empty database, so a Begin() that lands on a
	// second one sees no `nodes` table at all. Caught by
	// TestColumnIsGenerated_WorksThroughBothQueriers, which disagreed with
	// itself until this line existed. internal/fixturedb caps for the same
	// reason (there, to keep TEMP views alive).
	db.SetMaxOpenConns(1)
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

// TestColumnIsGenerated_ClassifiesByWritability pins what the probe is FOR:
// predicting whether a writer may name a column. A generated column is readable
// and indexed but rejected at PREPARE time, so the classification is asserted
// against the actual write failure rather than against the pragma alone.
func TestColumnIsGenerated_ClassifiesByWritability(t *testing.T) {
	db := openMixedColumnDB(t)

	assert.False(t, ColumnIsGenerated(db, "nodes", "id"), "ordinary column is writable")
	assert.False(t, ColumnIsGenerated(db, "nodes", "name"), "ordinary column is writable")
	assert.True(t, ColumnIsGenerated(db, "nodes", "parent_id"), "GENERATED ... VIRTUAL")
	assert.True(t, ColumnIsGenerated(db, "nodes", "byte_len"), "GENERATED ... STORED")

	assert.False(t, ColumnIsGenerated(db, "nodes", "no_such_column"), "absence is not generatedness")
	assert.False(t, ColumnIsGenerated(db, "no_such_table", "id"))

	_, err := db.Exec(`INSERT INTO nodes (id, name) VALUES ('a/b', 'b')`)
	require.NoError(t, err, "omitting the generated columns must succeed")

	_, err = db.Exec(`INSERT INTO nodes (id, parent_id, name) VALUES ('x/y', 'x', 'y')`)
	require.Error(t, err, "naming a generated column must fail")
	assert.Contains(t, err.Error(), "cannot INSERT into generated column")

	var parent string
	require.NoError(t, db.QueryRow(
		`SELECT parent_id FROM nodes WHERE id = 'a/b'`).Scan(&parent))
	assert.Equal(t, "a", parent, "the derived value is what a stored column would have held")
}

// TestColumnIsGenerated_WorksThroughBothQueriers is the reason RowQuerier exists.
// The probe is called from readers holding a *sql.DB and from writers mid
// transaction holding a *sql.Tx; before this package those were two separate
// implementations, which is two chances to answer differently.
func TestColumnIsGenerated_WorksThroughBothQueriers(t *testing.T) {
	db := openMixedColumnDB(t)
	cols := []string{"id", "name", "parent_id", "byte_len"}

	// Collected BEFORE the transaction opens. The fixture is capped at one
	// connection, so querying db while a tx holds it deadlocks rather than
	// disagreeing — which is how the first version of this test hung.
	viaDB := make(map[string]bool, len(cols))
	for _, c := range cols {
		viaDB[c] = ColumnIsGenerated(db, "nodes", c)
	}

	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	for _, c := range cols {
		assert.Equalf(t, viaDB[c], ColumnIsGenerated(tx, "nodes", c),
			"*sql.DB and *sql.Tx must agree about %s", c)
	}
	assert.True(t, ColumnIsGenerated(tx, "nodes", "parent_id"),
		"sanity: the shared answer is the correct one, not merely consistent")
}

// TestColumnIsGenerated_TableInfoWouldBeWrong documents the instrument this
// replaced, on the same table, so the difference it exists to preserve is
// stated rather than implied.
func TestColumnIsGenerated_TableInfoWouldBeWrong(t *testing.T) {
	db := openMixedColumnDB(t)

	seen := func(pragma, col string) bool {
		var n int
		require.NoError(t, db.QueryRow(
			"SELECT COUNT(*) FROM pragma_"+pragma+"('nodes') WHERE name = ?", col).Scan(&n))
		return n > 0
	}
	assert.True(t, seen("table_info", "id"), "sanity: table_info sees ordinary columns")
	assert.False(t, seen("table_info", "parent_id"), "table_info omits GENERATED VIRTUAL")
	assert.False(t, seen("table_info", "byte_len"), "table_info omits GENERATED STORED")
	assert.True(t, seen("table_xinfo", "parent_id"))
	assert.True(t, seen("table_xinfo", "byte_len"))
}
