package cmd

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/agentic-research/mache/graph"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingQuerier wraps a graph.RefsQuerier and counts QueryRefs calls whose SQL
// contains substr, so a test can assert setup work runs a fixed number of
// times regardless of how many rules are evaluated against it.
type countingQuerier struct {
	graph.RefsQuerier
	substr string
	count  int
}

func (c *countingQuerier) QueryRefs(query string, args ...any) (*sql.Rows, error) {
	if strings.Contains(query, c.substr) {
		c.count++
	}
	return c.RefsQuerier.QueryRefs(query, args...)
}

// TestEnsureSmellQueryContext_MaterializesViewsOncePerInvocation guards the
// perf fix: v_test_nodes (and v_doc_refs) are materialized as TEMP TABLEs,
// not views, specifically so their cost is paid once per find-smells
// invocation rather than once per rule (see ensureTestNodesView's doc
// comment). That invariant only holds if a multi-rule caller invokes
// ensureSmellQueryContext ONCE and then runSmellRuleQuery per rule.
// Regressing to calling runSmellRule (which redoes the setup) inside a
// rule loop silently re-pays the materialization cost per rule — measured
// on a real repo, that turned a ~14s find-smells --rule '*' --tags=gate run
// into ~81s, tripled again by the find-smells composite action's three
// invocations per PR.
func TestEnsureSmellQueryContext_MaterializesViewsOncePerInvocation(t *testing.T) {
	g := seedSmellAST(t)
	spy := &countingQuerier{RefsQuerier: g, substr: "CREATE TEMP TABLE v_test_nodes"}

	require.NoError(t, ensureSmellQueryContext(spy))

	for _, id := range []string{"magic_int_in_comparison", "sleep_in_test"} {
		rule := findRegisteredRule(t, id)
		_, err := runSmellRuleQuery(spy, rule, "", 100)
		require.NoError(t, err)
	}

	assert.Equal(t, 1, spy.count,
		"v_test_nodes must materialize once per invocation regardless of rule count")
}
