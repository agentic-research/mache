package smells

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gateRun runs one find-smells invocation and returns exit code + combined output.
func gateRun(t *testing.T, opts FindSmellsOptions) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	opts.Out, opts.Err = &buf, &buf
	code, err := RunFindSmells(opts)
	require.NoError(t, err)
	return code, buf.String()
}

// editBaseline rewrites the on-disk baseline through a mutator, so a test can
// express "a baseline written by a DIFFERENT backend or an older binary".
func editBaseline(t *testing.T, path string, mutate func(m map[string]any)) {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	mutate(m)
	out, err := json.MarshalIndent(m, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, out, 0o644))
}

// TestGate_GreenRunSaysWhatItDidNotAssess is the defect this bead names.
//
// A rule whose tables are absent skips, which is right — it cannot invent
// `_ast`. But it then reports zero findings, and zero findings is byte-
// identical to a rule that ran and found nothing. The ratchet printed "0 NEW"
// and the gate went green having assessed strictly less than it claimed: a
// disabled linter, arrived at politely.
func TestGate_GreenRunSaysWhatItDidNotAssess(t *testing.T) {
	db := writeSmellCLIFixture(t) // nodes/node_defs/node_refs, NO _ast
	base := filepath.Join(t.TempDir(), "b.json")

	code, _ := gateRun(t, FindSmellsOptions{DB: db, Rule: "*", WriteBaseline: base})
	require.Equal(t, 0, code)

	code, out := gateRun(t, FindSmellsOptions{DB: db, Rule: "*", Baseline: base})
	require.Equal(t, 0, code, "same backend as the baseline: no new debt, no coverage regression")
	assert.Contains(t, out, "0 NEW finding(s)")
	assert.Contains(t, out, "SKIPPED",
		"a green gate must state what it did not assess, or green cannot be read as clean")
	assert.Contains(t, out, "duplicate_code",
		"the skipped rules must be named — a bare count is not actionable")
	assert.Contains(t, out, "needs", "and why they skipped, so the fix is obvious")

	// The two degradations land on the same backends and compound: no `_ast`
	// means no node_hash, so the baseline silently falls back to path keying
	// and a moved file reads as new debt. Reported on the same lines, because
	// discovering one and not the other leaves the output unexplained —
	// observed live, where this line is what accounts for four "NEW findings"
	// that are really a keying mismatch.
	assert.Contains(t, out, "DEGRADED",
		"a producer with no node_hash silently path-keys the baseline; that must be stated too")
}

// TestGate_CoverageRegressionFails covers the dangerous case: the baseline was
// built with a rule's findings in it, so its counts encode debt that rule
// found. Gating without that rule compares against a baseline this run cannot
// honour, and reporting "0 NEW" would be the exact lie.
func TestGate_CoverageRegressionFails(t *testing.T) {
	db := writeSmellCLIFixture(t)
	base := filepath.Join(t.TempDir(), "b.json")
	code, _ := gateRun(t, FindSmellsOptions{DB: db, Rule: "*", WriteBaseline: base})
	require.Equal(t, 0, code)

	// Claim the baseline was produced on a backend where everything ran.
	editBaseline(t, base, func(m map[string]any) { m["rules_skipped"] = []any{} })

	code, out := gateRun(t, FindSmellsOptions{DB: db, Rule: "*", Baseline: base})
	assert.Equal(t, 1, code, "losing a rule that contributed to the baseline must fail the gate")
	assert.Contains(t, out, "COVERAGE REGRESSION")
	assert.Contains(t, out, "duplicate_code")
}

// TestGate_UnrecordedCoverageReportsButDoesNotFail pins the deliberate limit.
//
// A baseline written before coverage was recorded says nothing about what ran.
// Absence is not evidence of full coverage, and failing on an unknown would
// break every existing repo on upgrade — so the skip is reported and the gate
// still passes. Regenerating the baseline restores the stronger check.
func TestGate_UnrecordedCoverageReportsButDoesNotFail(t *testing.T) {
	db := writeSmellCLIFixture(t)
	base := filepath.Join(t.TempDir(), "b.json")
	code, _ := gateRun(t, FindSmellsOptions{DB: db, Rule: "*", WriteBaseline: base})
	require.Equal(t, 0, code)

	editBaseline(t, base, func(m map[string]any) { delete(m, "rules_skipped") })

	code, out := gateRun(t, FindSmellsOptions{DB: db, Rule: "*", Baseline: base})
	assert.Equal(t, 0, code, "an unrecorded baseline is unknown coverage, not proven-full coverage")
	assert.NotContains(t, out, "COVERAGE REGRESSION")
	assert.Contains(t, out, "SKIPPED", "but the skip is still reported — silence was the bug")
}

// TestCoverageRegression_ThreeStates separates the states the JSON encoding has
// to keep apart. `omitempty` collapsed "recorded: nothing skipped" and "not
// recorded" onto the same absent key, which disarmed the check in exactly the
// case it exists for.
func TestCoverageRegression_ThreeStates(t *testing.T) {
	now := []skippedRule{{ID: "duplicate_code", Missing: []string{"_ast"}}}

	assert.Equal(t, []string{"duplicate_code"},
		coverageRegression(now, smellBaseline{RulesSkipped: []string{}}),
		"baseline recorded full coverage: losing a rule is a regression")

	assert.Empty(t, coverageRegression(now, smellBaseline{RulesSkipped: []string{"duplicate_code"}}),
		"baseline recorded the same degradation: not a regression")

	assert.Empty(t, coverageRegression(now, smellBaseline{RulesSkipped: nil}),
		"baseline recorded nothing: unknown, so report rather than fail")
}

// TestBaseline_RulesSkippedSurvivesRoundTrip guards the encoding those three
// states depend on: an empty record must come back non-nil.
func TestBaseline_RulesSkippedSurvivesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name    string
		skipped []string
		wantNil bool
	}{
		{"full coverage", []string{}, false},
		{"a degradation", []string{"duplicate_code", "long_function"}, false},
		{"not recorded", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name+".json")
			b := computeBaseline([]smellFinding{rf("dead_code", "a.go", 1)})
			b.RulesSkipped = tc.skipped
			require.NoError(t, writeBaseline(path, b))

			got, err := loadBaseline(path)
			require.NoError(t, err)
			if tc.wantNil {
				assert.Nil(t, got.RulesSkipped, "unrecorded must stay unrecorded, not become empty")
			} else {
				require.NotNil(t, got.RulesSkipped, "a recorded coverage set must survive as non-nil")
				assert.Equal(t, tc.skipped, got.RulesSkipped)
			}
		})
	}
}

// TestSARIF_ReportsSkippedRules pins the SARIF half. A skipped rule emits no
// results, which in SARIF is indistinguishable from a rule that ran clean —
// code-scanning would show a green run for an analysis that never happened.
func TestSARIF_ReportsSkippedRules(t *testing.T) {
	doc := buildSARIFDoc(nil, "", []skippedRule{{ID: "duplicate_code", Missing: []string{"_ast"}}})
	raw, err := json.Marshal(doc)
	require.NoError(t, err)

	var got struct {
		Runs []struct {
			Invocations []struct {
				ExecutionSuccessful bool `json:"executionSuccessful"`
				Notifications       []struct {
					Level          string `json:"level"`
					AssociatedRule struct {
						ID string `json:"id"`
					} `json:"associatedRule"`
					Message struct {
						Text string `json:"text"`
					} `json:"message"`
				} `json:"toolExecutionNotifications"`
			} `json:"invocations"`
		} `json:"runs"`
	}
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Len(t, got.Runs, 1)
	require.Len(t, got.Runs[0].Invocations, 1)
	inv := got.Runs[0].Invocations[0]
	assert.True(t, inv.ExecutionSuccessful, "a degraded run is not a failed run")
	require.Len(t, inv.Notifications, 1)
	assert.Equal(t, "duplicate_code", inv.Notifications[0].AssociatedRule.ID)
	assert.Equal(t, "warning", inv.Notifications[0].Level)
	assert.Contains(t, inv.Notifications[0].Message.Text, "_ast")
	assert.Contains(t, inv.Notifications[0].Message.Text, "not known to be zero",
		"the message must say absent-not-zero, which is the whole distinction")
}
