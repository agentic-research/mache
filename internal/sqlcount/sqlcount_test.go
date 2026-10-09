package sqlcount_test

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/agentic-research/mache/internal/sqlcount"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestSQLCount_IsCalibrated checks the instrument before anything relies on
// it. A statement counter that silently misses the fast path, or counts each
// execution twice, would make every growth-class assertion built on it
// meaningless — and it would look like it was working.
func TestSQLCount_IsCalibrated(t *testing.T) {
	sqlcount.RegisterDriver()
	db, err := sql.Open(sqlcount.DriverName, ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1) // one conn, so nothing is executed on a second connection

	_, err = db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`)
	require.NoError(t, err)

	for _, n := range []int{1, 5, 20} {
		t.Run(fmt.Sprintf("%d statements", n), func(t *testing.T) {
			count := sqlcount.Reset()
			for i := range n {
				rows, err := db.Query(`SELECT v FROM t WHERE id = ?`, i)
				require.NoError(t, err)
				require.NoError(t, rows.Close())
			}
			assert.Equal(t, int64(n), count(),
				"exactly one count per query — no missed fast path, no double count")
		})
	}

	t.Run("writes are deliberately not counted", func(t *testing.T) {
		count := sqlcount.Reset()
		for i := range 4 {
			_, err := db.Exec(`INSERT INTO t (v) VALUES (?)`, fmt.Sprint(i))
			require.NoError(t, err)
		}
		assert.Equal(t, int64(0), count(),
			"only queries are counted — see the note in sqlcount.go on why writes are out")
	})

	t.Run("reset actually resets", func(t *testing.T) {
		count := sqlcount.Reset()
		assert.Equal(t, int64(0), count())
	})

	// The prepare counter's whole job is to tell these two apart, so both
	// halves are calibrated: a query STRING compiles every time it is issued,
	// and a prepared statement compiles once however often it runs. A counter
	// that could not separate them would make the read-path gate built on it
	// (mache-3063fb) pass whether or not statements were reused.
	t.Run("a query string compiles on every call", func(t *testing.T) {
		prepares := sqlcount.ResetPrepares()
		for i := range 7 {
			rows, err := db.Query(`SELECT v FROM t WHERE id = ?`, i)
			require.NoError(t, err)
			require.NoError(t, rows.Close())
		}
		assert.Equal(t, int64(7), prepares(), "one compilation per call when handed a string")
	})

	t.Run("a prepared statement compiles once", func(t *testing.T) {
		prepares := sqlcount.ResetPrepares()
		stmt, err := db.Prepare(`SELECT v FROM t WHERE id = ?`)
		require.NoError(t, err)
		t.Cleanup(func() { _ = stmt.Close() })
		for i := range 7 {
			rows, err := stmt.Query(i)
			require.NoError(t, err)
			require.NoError(t, rows.Close())
		}
		assert.Equal(t, int64(1), prepares(),
			"reusing a prepared statement on one connection must not compile again")
	})
}
