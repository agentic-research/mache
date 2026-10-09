package graph

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/fixturedb"
	"github.com/agentic-research/mache/internal/sqlcount"
)

// The direct read path must compile its SQL ONCE, not once per call
// (mache-3063fb).
//
// A CPU profile of a warm GetNode on a static .db, with page I/O out of the
// way, put SQLite's parser at about half of all remaining time: NodesTableReader
// handed database/sql a query STRING on every call, so modernc compiled the
// statement from scratch and threw it away — the same statement, thousands of
// times, inside every bulk walk (get_architecture BFSes up to 50,000 nodes).
//
// The assertion is a COUNT of compilations, not a duration, for the reasons
// internal/sqlcount gives: wall-clock cannot tell a slow runner from a wrong
// complexity class, and a count has no flake surface. And it is EXACT: after a
// warm-up, issuing an access N more times must compile ZERO new statements at
// every N. Anything above zero means some call is building SQL per invocation
// again.

// seededNodesDB returns the path of a nodes table holding one directory with
// several files under it — enough for every hot access to return real rows.
//
// It reuses newNodesDB for the schema rather than restating it, then hands back
// the FILE so the measuring connection can be opened through the counting
// driver. The path comes from SQLite itself (PRAGMA database_list) rather than
// from reconstructing newNodesDB's temp-dir layout.
func seededNodesDB(t *testing.T) string {
	t.Helper()
	db := newNodesDB(t, false, false)

	insert := `INSERT INTO nodes (id, parent_id, name, kind, size, mtime, record_id, record, source_file)
		VALUES (?, ?, ?, ?, ?, 0, '', '', ?)`
	_, err := db.Exec(insert, "pkg", "", "pkg", NodeKindDir, 0, "")
	require.NoError(t, err)
	for i := range 5 {
		name := fmt.Sprintf("f%d.go", i)
		_, err = db.Exec(insert, "pkg/"+name, "pkg", name, NodeKindFile, 10*(i+1), "/src/pkg/"+name)
		require.NoError(t, err)
		_, err = db.Exec(`UPDATE nodes SET record = ? WHERE id = ?`, "content of "+name, "pkg/"+name)
		require.NoError(t, err)
	}

	var seq int
	var schema, path string
	require.NoError(t, db.QueryRow("PRAGMA database_list").Scan(&seq, &schema, &path))
	require.NotEmpty(t, path, "a file-backed db must report its path")
	return path
}

func TestNodesTableReader_CompilesEachStatementOnce(t *testing.T) {
	sqlcount.RegisterDriver()
	path := seededNodesDB(t)

	db, err := sql.Open(sqlcount.DriverName, path+"?mode=ro")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	// ONE connection, so the count is exact. The property under test — that
	// compilations do not scale with calls — holds at any pool size, but a
	// prepared statement is compiled once PER connection it lands on, so a
	// pool would make the expected count depend on scheduling.
	db.SetMaxOpenConns(1)

	r := NewNodesTableReader(db, "nodes", nil, nil, 0o444, 0o555, 64)
	t.Cleanup(r.Close)

	files := []string{"pkg/f0.go", "pkg/f1.go", "pkg/f2.go", "pkg/f3.go", "pkg/f4.go"}

	// A one-entry content cache, so ReadContent cycling five files MISSES every
	// time and actually reaches SQL. With the default cache every repeat read
	// is served from memory, and "zero compilations" would be true for a
	// reason that has nothing to do with statement reuse.
	uncached := NewNodesTableReader(db, "nodes", nil, nil, 0o444, 0o555, 1)
	t.Cleanup(uncached.Close)
	buf := make([]byte, 64)

	for _, tc := range []struct {
		access string
		call   func(i int) error
	}{
		{"GetNode", func(i int) error { _, err := r.GetNode(files[i%len(files)]); return err }},
		{"ListChildren", func(int) error { _, err := r.ListChildren("pkg"); return err }},
		{"ListChildren(root)", func(int) error { _, err := r.ListChildren(""); return err }},
		{"ListChildStats", func(int) error { _, err := r.ListChildStats("pkg"); return err }},
		{"ListChildStats(root)", func(int) error { _, err := r.ListChildStats(""); return err }},
		{"ReadContent", func(i int) error {
			_, err := uncached.ReadContent(files[i%len(files)], buf, 0)
			return err
		}},
	} {
		t.Run(tc.access, func(t *testing.T) { assertCompilesOnce(t, tc.access, tc.call) })
	}
}

// GetCallers reads node_refs, which ley-line-open owns, so its fixture comes
// from fixturedb rather than a hand-written CREATE TABLE — the LLO boundary
// gate forbids the latter, and rightly: hand-typed DDL decides which shape the
// reader is tested against (mache-7555da).
func TestNodesTableReader_GetCallersCompilesOnce(t *testing.T) {
	sqlcount.RegisterDriver()

	b := fixturedb.New(t, fixturedb.Standalone)
	b.Construct("pkg/caller")
	b.Construct("pkg/other")
	b.Ref("Target", "pkg/caller", "", "")
	b.Ref("Target", "pkg/other", "", "")
	path, _ := b.Build()

	db, err := sql.Open(sqlcount.DriverName, path+"?mode=ro")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	r := NewNodesTableReader(db, "nodes", nil, nil, 0o444, 0o555, 64)
	t.Cleanup(r.Close)

	callers, err := r.GetCallers("Target")
	require.NoError(t, err)
	require.Len(t, callers, 2, "the fixture must yield real callers, or zero compilations proves nothing")

	assertCompilesOnce(t, "GetCallers", func(int) error { _, err := r.GetCallers("Target"); return err })
}

// assertCompilesOnce warms an access, then issues it 10, 100 and 1000 more
// times and requires ZERO new compilations at every N.
func assertCompilesOnce(t *testing.T, access string, call func(i int) error) {
	t.Helper()
	// Warm-up: whatever is compiled on first use is out of the measurement,
	// so the number is the steady-state cost.
	require.NoError(t, call(0))

	for _, n := range []int{10, 100, 1000} {
		prepares := sqlcount.ResetPrepares()
		for i := range n {
			require.NoError(t, call(i))
		}
		assert.Equalf(t, int64(0), prepares(),
			"%s compiled %d statements across %d warm calls; it must compile ZERO — "+
				"a prepared statement is reused, a query string is recompiled every time",
			access, prepares(), n)
	}
}
