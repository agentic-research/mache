package lloschema

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/agentic-research/mache/internal/fixturedb"
)

// fieldChildPlan is the query plan of every _ast row LEFT JOINed to its
// fielded children through FieldChildJoins.
func fieldChildPlan(t *testing.T, p fixturedb.Producer) string {
	t.Helper()
	db := paritySpec(t, p).DB()
	joins, err := FieldChildJoins(DB(db), "a.node_hash")
	require.NoError(t, err)
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

// On projection-v9 node_child is a view, and SQLite materializes a join view
// whole on the right of a LEFT JOIN, so the joins go to node_edge: the plan
// probes node_edge by its key and never builds node_child (mache-72668d).
func TestFieldChildJoins_ProbesNodeEdgeOnV9(t *testing.T) {
	plan := fieldChildPlan(t, fixturedb.LeylineV6)
	assert.NotContains(t, plan, "MATERIALIZE", plan)
	assert.NotContains(t, plan, "node_child", plan)
	assert.Contains(t, plan, "SEARCH ce USING PRIMARY KEY (parent_id=?)", plan)
}

// An arena from before projection-v9 holds node_child as the table, and has no
// node_edge to join.
func TestFieldChildJoins_ReadsTheNodeChildTableBeforeV9(t *testing.T) {
	plan := fieldChildPlan(t, fixturedb.Leyline)
	assert.Contains(t, plan, "SEARCH ce USING", plan)
	assert.NotContains(t, plan, "node_edge", plan)
}
