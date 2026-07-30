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

// Validates the containment computation against the independently-derived
// SQL figure: at an 80-line window over this repo, 96.5% of constructs fit.
// A construct exactly at the window size fits; one line over does not.
func TestComputeContainment_BoundaryIsInclusive(t *testing.T) {
	c := computeContainment(80, []int{79, 80, 81}, 0)
	assert.Equal(t, 2, c.Contained, "79 and 80 fit; 81 does not")
	assert.Equal(t, 1, c.Overflows)
	assert.InDelta(t, 66.67, c.ContainedPct, 0.01)
}

// Aligned vs unaligned is the whole argument: a cap that knows where the
// construct starts needs ~1 read; a content-blind cap needs
// median_file_lines/window. The GAP between them is the value of the index.
func TestComputeContainment_AlignedAndUnalignedDiverge(t *testing.T) {
	lines := make([]int, 100)
	for i := range lines {
		lines[i] = 16 // p50 construct size measured on this repo
	}
	c := computeContainment(80, lines, 162) // median file measured on this repo
	assert.InDelta(t, 100.0, c.ContainedPct, 0.01, "16-line constructs all fit an 80-line window")
	assert.InDelta(t, 1.0, c.AlignedReads, 0.01, "aligned: one read each")
	assert.InDelta(t, 2.02, c.UnalignedReads, 0.02, "content-blind: ~2 reads, 162/80")
	assert.Greater(t, c.UnalignedReads, c.AlignedReads*1.9,
		"the alignment gap is the measurable value of knowing where the construct is")
}

// A window at least as large as the median file needs no second read.
func TestComputeContainment_WindowCoveringFileNeedsOneRead(t *testing.T) {
	c := computeContainment(320, []int{16, 20}, 162)
	assert.InDelta(t, 1.0, c.UnalignedReads, 0.001)
}

// An oversized construct costs ceil(lines/window) reads even when aligned.
func TestComputeContainment_OverflowCostsCeilReads(t *testing.T) {
	c := computeContainment(80, []int{384}, 0) // the longest construct in this repo
	assert.Equal(t, 0, c.Contained)
	assert.InDelta(t, 5.0, c.AlignedReads, 0.001, "ceil(384/80) = 5")
}

// The closed form of the argument: a line cap can have the byte saving OR one
// round trip, never both. grep-then-read is the cap's BEST realistic policy and
// it is structurally >= 2 calls at every window, because locating and reading
// are separate steps. Construct-granular retrieval collapses them: the search
// IS the retrieval.
func TestComputeContainment_CapNeverBeatsTwoRoundTripsWhileSaving(t *testing.T) {
	lines := make([]int, 100)
	for i := range lines {
		lines[i] = 16
	}
	for _, w := range []int{20, 40, 80, 160} {
		c := computeContainment(w, lines, 162)
		assert.GreaterOrEqual(t, c.GrepThenReadReads, 2.0,
			"grep-then-read is always a locate plus a read")
		assert.Less(t, c.AlignedReads, c.GrepThenReadReads,
			"knowing where the construct is always beats having to find it")
	}
	// Only a window that swallows the file reaches one unaligned read — and at
	// that point the byte saving is gone.
	big := computeContainment(320, lines, 162)
	assert.InDelta(t, 1.0, big.UnalignedReads, 0.001)
}

// Per-ANSWER is the unit a reader acts on, and it inverts the per-read story.
// A policy that halves bytes but doubles round trips has saved nothing; the
// earlier "a line cap captures 28.9 of 57.4 points" was true per read and is
// ~0 per answer. Same error class as ratio-of-means: right arithmetic, wrong
// unit.
func TestPerAnswer_HalfBytesTwiceTheReadsSavesNothing(t *testing.T) {
	p := perAnswer("W_line_cap", 2.01, 2.05, 0.574)
	assert.InDelta(t, 0.98, p.EffectiveRatio, 0.01)
	assert.Zero(t, p.ReductionPct, "effective ratio below 1 is no saving at all")
	assert.Zero(t, p.PctOfCeiling)
}

// Construct-granular retrieval keeps almost all of its per-read advantage,
// because it does not pay a positioning round trip — the search IS the read.
func TestPerAnswer_ConstructKeepsItsAdvantage(t *testing.T) {
	p := perAnswer("B_construct", 26.09, 1.05, 0.574)
	assert.InDelta(t, 24.85, p.EffectiveRatio, 0.05) // 26.09/1.05
	assert.Greater(t, p.PctOfCeiling, 95.0)
}

// Arm A is the unit: one read, whole file, zero reduction by definition.
func TestPerAnswer_WholeFileIsTheUnit(t *testing.T) {
	p := perAnswer("A_whole_file", 1, 1, 0.574)
	assert.InDelta(t, 1.0, p.EffectiveRatio, 1e-9)
	assert.Zero(t, p.ReductionPct)
}
