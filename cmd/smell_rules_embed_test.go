package cmd

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// priorBuiltinRoster is the exact set of built-in rule IDs as they
// stood when the registry was a hand-written []SmellRule literal
// (before mache-b0b979 externalized them to cmd/rules/*.json). It is
// the frozen roster the embedded set MUST reproduce — a rule dropped
// or a stray file added would drift from this list and fail the test.
var priorBuiltinRoster = []string{
	"magic_int_in_comparison",
	"dead_code",
	"cyclomatic_complexity",
	"long_function",
	"untested_function",
	"duplicate_definitions",
	"duplicate_code",
	"god_file",
	"fan_out_skew",
	"sleep_in_test",
	"long_file",
	"drift_doc_dead_symbol_reference",
	"drift_doc_broken_internal_link",
	"drift_doc_outdated_count",
}

// TestEmbeddedRules_IDsMatchPriorRoster asserts the embedded builtin
// set is exactly the roster that shipped as struct literals — same
// count, same IDs. This is the behavior-preservation anchor: if a
// cmd/rules/*.json is deleted, renamed, or an extra one is added, the
// set diverges and this fails.
func TestEmbeddedRules_IDsMatchPriorRoster(t *testing.T) {
	got := make([]string, 0, len(smellRegistry))
	for i := range smellRegistry {
		got = append(got, smellRegistry[i].ID)
	}

	want := append([]string(nil), priorBuiltinRoster...)
	sort.Strings(want)
	gotSorted := append([]string(nil), got...)
	sort.Strings(gotSorted)

	assert.Equal(t, want, gotSorted,
		"embedded builtin rule IDs must equal the prior struct-literal roster (count + IDs)")
	assert.Len(t, smellRegistry, len(priorBuiltinRoster),
		"embedded builtin count must equal the prior roster count")
}

// TestEmbeddedRules_EveryRuleValidates asserts each embedded rule
// satisfies the same contract external rules must — non-empty Query,
// exactly one '%s' scope placeholder, and passing validateSmellRule
// (which also checks the fmt-safety of the query and the ScopeColumn
// whitelist). mustLoadEmbeddedRules already panics on a violation at
// init, but this makes the guarantee an explicit, named assertion.
func TestEmbeddedRules_EveryRuleValidates(t *testing.T) {
	for i := range smellRegistry {
		r := smellRegistry[i]
		t.Run(r.ID, func(t *testing.T) {
			assert.NotEmpty(t, r.Query, "rule %q Query must be non-empty", r.ID)
			assert.Equal(t, 1, strings.Count(r.Query, "%s"),
				"rule %q Query must contain exactly one '%%s' scope placeholder", r.ID)
			assert.NoError(t, validateSmellRule(r),
				"rule %q must satisfy the shared SmellRule validation contract", r.ID)
		})
	}
}

// TestEmbeddedRules_LoadOrderDeterministic asserts the embedded loader
// returns rules in a stable, filename-sorted order across repeated
// loads. Rule order is user-visible (listing / help output), so a
// non-deterministic map iteration would churn output and flake tests.
func TestEmbeddedRules_LoadOrderDeterministic(t *testing.T) {
	first, err := loadRuleFS(embeddedRulesFS, "rules")
	require.NoError(t, err)
	require.NotEmpty(t, first)

	firstIDs := make([]string, len(first))
	for i := range first {
		firstIDs[i] = first[i].ID
	}

	// Sorted-by-ID is the contract (files are named <ID>.json).
	sorted := append([]string(nil), firstIDs...)
	sort.Strings(sorted)
	assert.Equal(t, sorted, firstIDs, "embedded rules must load in filename/ID-sorted order")

	// Repeated loads are identical.
	for n := 0; n < 3; n++ {
		again, err := loadRuleFS(embeddedRulesFS, "rules")
		require.NoError(t, err)
		againIDs := make([]string, len(again))
		for i := range again {
			againIDs[i] = again[i].ID
		}
		assert.Equal(t, firstIDs, againIDs, "embedded load order must be stable across loads")
	}

	// The package-global registry reflects the same order.
	regIDs := make([]string, len(smellRegistry))
	for i := range smellRegistry {
		regIDs[i] = smellRegistry[i].ID
	}
	assert.Equal(t, firstIDs, regIDs, "smellRegistry must mirror the embedded load order")
}
