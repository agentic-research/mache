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
}
