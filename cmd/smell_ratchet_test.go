package cmd

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rf builds a minimal smellFinding for ratchet tests (only rule/file/line matter).
func rf(rule, src string, line int) smellFinding {
	return smellFinding{RuleID: rule, SourceID: src, Line: line}
}

func TestSmellBaseline_ComputeAndLookup(t *testing.T) {
	base := computeBaseline([]smellFinding{
		rf("long_function", "a.go", 10),
		rf("long_function", "a.go", 40),
		rf("long_function", "b.go", 5),
		rf("magic_int", "a.go", 3),
	})
	assert.Equal(t, 2, base.lookup("long_function", "a.go"))
	assert.Equal(t, 1, base.lookup("long_function", "b.go"))
	assert.Equal(t, 1, base.lookup("magic_int", "a.go"))
	assert.Equal(t, 0, base.lookup("long_function", "c.go"), "unknown file → 0")
	assert.Equal(t, 0, base.lookup("dead_code", "a.go"), "unknown rule → 0")
}

func TestSmellBaseline_JSONRoundTrip_Deterministic(t *testing.T) {
	orig := computeBaseline([]smellFinding{
		rf("long_function", "a.go", 10),
		rf("long_function", "a.go", 40),
		rf("magic_int", "z.go", 3),
	})
	blob, err := json.Marshal(orig)
	require.NoError(t, err)

	var got smellBaseline
	require.NoError(t, json.Unmarshal(blob, &got))
	assert.Equal(t, 2, got.lookup("long_function", "a.go"))
	assert.Equal(t, 1, got.lookup("magic_int", "z.go"))

	// Output must be order-independent so a committed baseline file doesn't
	// churn on every regeneration.
	shuffled, err := json.Marshal(computeBaseline([]smellFinding{
		rf("magic_int", "z.go", 3),
		rf("long_function", "a.go", 40),
		rf("long_function", "a.go", 10),
	}))
	require.NoError(t, err)
	assert.JSONEq(t, string(blob), string(shuffled), "baseline serialization must be deterministic")
}

func TestNewDebt(t *testing.T) {
	baseline := computeBaseline([]smellFinding{
		rf("long_function", "a.go", 10),
		rf("long_function", "a.go", 40),
		rf("magic_int", "a.go", 3),
	})

	t.Run("no change → no debt", func(t *testing.T) {
		cur := []smellFinding{rf("long_function", "a.go", 12), rf("long_function", "a.go", 44), rf("magic_int", "a.go", 3)}
		assert.Empty(t, newDebt(cur, baseline))
	})

	t.Run("improvement (fewer) → no debt", func(t *testing.T) {
		cur := []smellFinding{rf("long_function", "a.go", 12)} // was 2, now 1
		assert.Empty(t, newDebt(cur, baseline))
	})

	t.Run("+1 in existing (rule,file) → exactly 1 debt", func(t *testing.T) {
		cur := []smellFinding{rf("long_function", "a.go", 12), rf("long_function", "a.go", 44), rf("long_function", "a.go", 88)}
		d := newDebt(cur, baseline)
		require.Len(t, d, 1)
		assert.Equal(t, "long_function", d[0].RuleID)
		assert.Equal(t, "a.go", d[0].SourceID)
	})

	t.Run("new (rule,file) → all its findings are debt", func(t *testing.T) {
		cur := []smellFinding{rf("dead_code", "new.go", 1), rf("dead_code", "new.go", 9)}
		d := newDebt(cur, baseline)
		require.Len(t, d, 2)
		for _, x := range d {
			assert.Equal(t, "dead_code", x.RuleID)
		}
	})

	t.Run("empty baseline → all findings are debt", func(t *testing.T) {
		cur := []smellFinding{rf("x", "a", 1), rf("y", "b", 2)}
		assert.Len(t, newDebt(cur, smellBaseline{}), 2)
	})

	t.Run("deterministic output", func(t *testing.T) {
		cur := []smellFinding{rf("long_function", "a.go", 88), rf("long_function", "a.go", 12), rf("long_function", "a.go", 44), rf("long_function", "a.go", 60)}
		d1 := newDebt(cur, baseline)
		d2 := newDebt(cur, baseline)
		assert.Equal(t, d1, d2)
		assert.Len(t, d1, 2, "baseline 2, current 4 → 2 new")
	})
}
