package smells

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSmellScan_MaterializesViewsOncePerScan guards the perf fix:
// v_test_nodes (and v_doc_refs) are materialized as TEMP TABLEs, not views,
// specifically so their cost is paid once per scan rather than once per rule
// (see ensureTestNodesView's doc comment). Re-materializing per rule turned a
// ~14s find-smells --rule '*' --tags=gate run into ~81s on a real repo
// (mache-808b0b). A marker row added after the scan opens survives every
// rule the scan runs only if no rule rebuilds the table.
func TestSmellScan_MaterializesViewsOncePerScan(t *testing.T) {
	s := scanOf(t, seedSmellAST(t))
	rows, err := s.QueryRefs(`INSERT INTO temp.v_test_nodes (node_id) VALUES ('marker')`)
	require.NoError(t, err)
	require.NoError(t, rows.Close())

	for _, id := range []string{"magic_int_in_comparison", "sleep_in_test"} {
		_, err := runSmellRuleQuery(s, findRegisteredRule(t, id), "", 100)
		require.NoError(t, err)
	}

	var n int
	require.NoError(t, s.conn.QueryRowContext(s.ctx,
		`SELECT count(*) FROM temp.v_test_nodes WHERE node_id = 'marker'`).Scan(&n))
	assert.Equal(t, 1, n, "a rule re-materialized v_test_nodes inside the scan")
}
