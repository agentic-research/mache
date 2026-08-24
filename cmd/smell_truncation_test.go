package cmd

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSmellResponse_SignalsTruncation covers the defect in mache-2b1ea7: the
// envelope reported the length of the TRUNCATED slice as `total`, so a caller
// could not distinguish "this repo has 200 findings" from "here are the first
// 200 of some larger number".
//
// That is not theoretical. Measured on mache's own 862-file tree at the default
// cap of 200: cyclomatic_complexity has 3,980 findings, magic_int_in_comparison
// 1,430, duplicate_code 340 — three of fourteen rules, under-reported by up to
// 20x, with nothing in the payload saying so.
func TestSmellResponse_SignalsTruncation(t *testing.T) {
	mk := func(n int) []smellFinding {
		out := make([]smellFinding, n)
		for i := range out {
			out[i] = smellFinding{RuleID: "r", SourceID: "a.go"}
		}
		return out
	}

	for _, tc := range []struct {
		name    string
		n       int
		limit   int
		want    bool
		because string
	}{
		{"well under the cap", 12, 200, false, "a complete result must not warn"},
		{"empty", 0, 200, false, "a clean rule must keep its existing shape"},
		{
			"exactly at the cap", 200, 200, true,
			"indistinguishable from truncation, and over-warning is the safe direction",
		},
		{
			"more rows than the cap", 250, 200, true,
			"a caller bug, but answering \"complete\" for it is the worst available outcome",
		},
		{
			"uncapped", 3980, noSmellLimit, false,
			"no cap means truncation is impossible, however many findings there are",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := newSmellResponse("r", mk(tc.n), tc.limit)
			assert.Equal(t, tc.n, resp.Total)
			assert.Equal(t, tc.want, resp.Truncated, tc.because)
		})
	}
}

// TestSmellResponse_TruncatedIsOmittedWhenFalse pins the wire shape. The field
// is additive on purpose — existing consumers of `total`/`findings` are
// unaffected, and a clean result serializes exactly as it did before.
func TestSmellResponse_TruncatedIsOmittedWhenFalse(t *testing.T) {
	clean, err := json.Marshal(newSmellResponse("r", nil, 200))
	require.NoError(t, err)
	assert.NotContains(t, string(clean), "truncated",
		"a complete result must serialize as it did before this field existed")
	assert.Contains(t, string(clean), `"findings":[]`,
		"and must keep the nil -> [] normalization")

	capped := make([]smellFinding, 2)
	hit, err := json.Marshal(newSmellResponse("r", capped, 2))
	require.NoError(t, err)
	assert.Contains(t, string(hit), `"truncated":true`,
		"a capped result must say so in the payload, not only in a log")
}
