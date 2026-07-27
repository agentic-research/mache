package main

// Retrieval-reduction ceiling, derived per-corpus rather than asserted.
//
// The commoditization result from the 2026-07-27 cost analysis is algebra, not
// an empirical coincidence. If retrieval-shaped tool output is a fraction S of
// carry-weighted context cost, and a better index shrinks that output by a
// factor R, session-level reduction is:
//
//	reduction(R) = S * (1 - 1/R)
//
// Two consequences fall straight out, and both are why mache should not
// compete on token reduction:
//
//  1. reduction is bounded above by S no matter how good R gets. You cannot
//     beat S by improving retrieval, because S is retrieval's entire share.
//  2. the curve is asymptotic in R. At R=10 you are already at 90% of the
//     ceiling; every further doubling buys a few percent. A 6x spread in
//     retrieval quality collapses to a few points of session cost.
//
// S is a property of a CORPUS, not of mache — a codebase whose agents mostly
// run tests has a lower S than one whose agents mostly read. So it is derived
// from that corpus's own transcripts here rather than hardcoded, and the
// ceiling travels with the measurement instead of being quoted from a thread.

// Ceiling reports the retrieval-reduction bound for one corpus.
type Ceiling struct {
	// ReadShare is S: retrieval-shaped tool output as a fraction of
	// carry-weighted context cost (block size x turns it is re-read).
	ReadShare float64 `json:"read_share"`
	// CeilingPct is the maximum achievable session-level reduction, = S.
	CeilingPct float64 `json:"ceiling_pct"`
	// Curve is reduction(R) at representative retrieval ratios.
	Curve []CurvePoint `json:"curve"`
	// R90 is the ratio at which reduction reaches 90% of the ceiling —
	// past here, better retrieval is nearly free of session-level effect.
	R90 float64 `json:"r90"`
	// Note carries the interpretation so a reader of the JSON alone cannot
	// mistake a big R for a big win.
	Note string `json:"note"`
}

// CurvePoint is one (ratio, reduction) sample.
type CurvePoint struct {
	Ratio        float64 `json:"ratio"`
	ReductionPct float64 `json:"reduction_pct"`
	PctOfCeiling float64 `json:"pct_of_ceiling"`
}

// reductionAt returns session-level reduction (as a percentage) for a
// retrieval-compression factor of r, given retrieval share s (0..1).
// r <= 1 means no compression, so no reduction.
func reductionAt(s, r float64) float64 {
	if r <= 1 {
		return 0
	}
	return s * (1 - 1/r) * 100
}

// computeCeiling derives the bound and the asymptote curve from a measured
// retrieval share. ratios are the sample points; the caller passes the
// corpus's own measured ratio among them so the artifact shows where this
// repo actually sits on the curve.
func computeCeiling(readShare float64, ratios []float64) Ceiling {
	c := Ceiling{
		ReadShare:  readShare,
		CeilingPct: readShare * 100,
		R90:        10, // reduction(10)/ceiling = 1 - 1/10 = 0.9 exactly, for any S
		Note: "Session-level reduction is bounded by read_share and asymptotic in the retrieval ratio: " +
			"reduction(R) = read_share * (1 - 1/R). At R=10 you are already at 90% of the ceiling, so a large " +
			"spread in retrieval quality collapses to a few points of session cost. Token reduction is therefore " +
			"a commoditized claim — evaluate on correctness-of-write and cross-format reach instead.",
	}
	for _, r := range ratios {
		red := reductionAt(readShare, r)
		pct := 0.0
		if c.CeilingPct > 0 {
			pct = red / c.CeilingPct * 100
		}
		c.Curve = append(c.Curve, CurvePoint{Ratio: r, ReductionPct: red, PctOfCeiling: pct})
	}
	return c
}

// PerAnswer converts a per-READ byte ratio into the per-ANSWER ratio a reader
// should actually act on, by charging the round trips each policy needs.
//
// This corrects an error of the same class as reporting ratio-of-means instead
// of the paired median: the arithmetic was right for a unit nobody cares about.
// "A line cap captures 28.9 of 57.4 available points" is true per read and
// close to false per answer, because the cap buys its byte saving by covering
// less of the file and pays it straight back locating the construct.
//
//	effective_ratio = byte_ratio / reads_per_answer
//
// Arm A is the unit: one read, full file. A policy needing 2 reads at half the
// bytes has an effective ratio of ~1.0 and has saved nothing.
type PerAnswer struct {
	Arm            string  `json:"arm"`
	ByteRatio      float64 `json:"byte_ratio"`       // vs whole-file read
	ReadsPerAnswer float64 `json:"reads_per_answer"` // round trips to a complete construct
	EffectiveRatio float64 `json:"effective_ratio"`  // byte_ratio / reads
	ReductionPct   float64 `json:"reduction_pct"`    // session-level, at this read_share
	PctOfCeiling   float64 `json:"pct_of_ceiling"`
}

func perAnswer(arm string, byteRatio, reads, readShare float64) PerAnswer {
	p := PerAnswer{Arm: arm, ByteRatio: byteRatio, ReadsPerAnswer: reads}
	if reads > 0 {
		p.EffectiveRatio = byteRatio / reads
	}
	p.ReductionPct = reductionAt(readShare, p.EffectiveRatio)
	if readShare > 0 {
		p.PctOfCeiling = p.ReductionPct / (readShare * 100) * 100
	}
	return p
}
