package lloschema

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/fixturedb"
)

// queryPlan is the query plan of every _ast row of db LEFT JOINed through
// joins.
func queryPlan(t *testing.T, db *sql.DB, joins string) string {
	t.Helper()
	rows, err := db.Query(`EXPLAIN QUERY PLAN SELECT a.node_hash, ce.field, cc.raw_kind FROM _ast a ` + joins)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		plan = append(plan, detail)
	}
	require.NoError(t, rows.Err())
	return strings.Join(plan, "\n")
}

// TestFieldChildJoins reads each projection's edges from the table that holds
// them. On projection-v9 node_child is a view, and SQLite materializes a join
// view whole on the right of a LEFT JOIN, so the joins probe node_edge by its
// key and never build node_child (mache-72668d). An arena from before v9 holds
// node_child as the table and has no node_edge to join.
func TestFieldChildJoins(t *testing.T) {
	for _, tc := range []struct {
		name     string
		producer fixturedb.Producer
		probe    string
		absent   []string
	}{
		{"projection-v9", fixturedb.LeylineV6, "SEARCH ce USING PRIMARY KEY (parent_id=?)", []string{"MATERIALIZE", "node_child"}},
		{"before v9", fixturedb.Leyline, "SEARCH ce USING", []string{"node_edge"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := paritySpec(t, tc.producer).DB()
			joins, err := FieldChildJoins(DB(db), "a.node_hash")
			require.NoError(t, err)
			plan := queryPlan(t, db, joins)
			assert.Contains(t, plan, tc.probe, plan)
			for _, a := range tc.absent {
				assert.NotContains(t, plan, a, plan)
			}
		})
	}
}
