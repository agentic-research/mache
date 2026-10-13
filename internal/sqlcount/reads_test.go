package sqlcount_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/internal/sqlcount"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The byte counter, calibrated against the two plans it exists to tell apart:
// a point lookup by primary key reads one root-to-leaf path of pages, and a
// scan reads every page of the table. If the counter missed reads, or counted
// something other than the file, the two would not come out in proportion to
// the table.
func TestReadCounted_SeparatesALookupFromAScan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	w, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = w.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`)
	require.NoError(t, err)
	_, err = w.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 20000)
		INSERT INTO t SELECT i, printf('%0100d', i) FROM n`)
	require.NoError(t, err)
	var pageSize, tablePages int64
	require.NoError(t, w.QueryRow(`PRAGMA page_size`).Scan(&pageSize))
	require.NoError(t, w.QueryRow(`PRAGMA page_count`).Scan(&tablePages))
	require.NoError(t, w.Close())

	measure := func(query string) int64 {
		rc, err := sqlcount.OpenReadCounted(path)
		require.NoError(t, err)
		defer func() { require.NoError(t, rc.Close()) }()
		// Schema first, so neither measurement includes reading it.
		require.NoError(t, rc.DB.QueryRow(`SELECT count(*) FROM sqlite_master`).Scan(new(int)))
		n := rc.Reset()
		var v string
		require.NoError(t, rc.DB.QueryRow(query).Scan(&v))
		return n()
	}

	lookup := measure(`SELECT v FROM t WHERE id = 12345`)
	scan := measure(`SELECT max(v) FROM t`)

	assert.Positive(t, lookup, "a lookup reads pages")
	assert.LessOrEqual(t, lookup, 4*pageSize, "a lookup reads one path from the root, a few pages")
	assert.GreaterOrEqual(t, scan, (tablePages-2)*pageSize, "a scan reads the whole table")
}
