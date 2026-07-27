package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The ceiling is read_share itself: no retrieval improvement, however large,
// can reduce session cost by more than retrieval's own share of it.
func TestReductionAt_BoundedByReadShare(t *testing.T) {
	const s = 0.574
	for _, r := range []float64{2, 10, 100, 1e6, 1e12} {
		assert.Less(t, reductionAt(s, r), s*100+1e-9,
			"reduction must never exceed the read share")
	}
	assert.InDelta(t, 57.4, reductionAt(s, 1e12), 0.01, "asymptotes to the ceiling")
}

// r <= 1 is no compression, so no reduction — guards against a caller passing
// a ratio of 0 or 1 and getting a negative or infinite "win".
func TestReductionAt_NoCompressionIsNoReduction(t *testing.T) {
	assert.Zero(t, reductionAt(0.574, 1))
	assert.Zero(t, reductionAt(0.574, 0))
	assert.Zero(t, reductionAt(0.574, -5))
}

// The commoditization result, stated as a test: at R=10 you are at exactly 90%
// of the ceiling REGARDLESS of read_share, because 1 - 1/R is independent of S.
// This is why a 6x spread in retrieval quality collapses to a few points of
// session cost, and why "fewer tokens" cannot be a differentiator.
func TestComputeCeiling_R90IsShareIndependent(t *testing.T) {
	for _, s := range []float64{0.2, 0.574, 0.9} {
		c := computeCeiling(s, []float64{10})
		assert.InDelta(t, 90.0, c.Curve[0].PctOfCeiling, 1e-9,
			"R=10 is 90%% of ceiling for every read_share")
	}
}

// Past the knee, large retrieval gains buy almost nothing. 26x -> 100x is a
// ~4x better index for ~1.6 points of session cost.
func TestComputeCeiling_DiminishingReturnsPastTheKnee(t *testing.T) {
	c := computeCeiling(0.574, []float64{26.1, 100})
	gain := c.Curve[1].ReductionPct - c.Curve[0].ReductionPct
	assert.Less(t, gain, 2.0, "quadrupling the retrieval ratio buys under 2 points")
	assert.Greater(t, gain, 0.0, "but it is still a gain, not a regression")
}

// The note must ship in the artifact: a reader who sees only the JSON must not
// be able to mistake a large ratio for a large win.
func TestComputeCeiling_NoteWarnsAgainstCompetingOnTokens(t *testing.T) {
	c := computeCeiling(0.574, []float64{10})
	assert.Contains(t, c.Note, "commoditized")
	assert.Contains(t, c.Note, "correctness-of-write")
}
