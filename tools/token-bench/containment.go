package main

import "sort"

// Semantic containment: whether a fixed-size line window can return a COMPLETE
// construct, rather than a fragment cut at an arbitrary boundary.
//
// This is the measurable form of "a line cap is not semantically viable." A
// cap is content-blind — it cuts at line N whether that is mid-function,
// mid-struct, or three lines into a doc comment — so the agent may receive a
// fragment that does not contain the thing it asked for, read again at a
// guessed offset, and pay both the extra retrieval and the reasoning to choose
// where to look next. Construct boundaries are semantically closed; line
// windows are arbitrary.
//
// Two numbers, and conflating them is the trap:
//
//   - ALIGNED containment assumes the window starts at the construct's first
//     line. That is the best case, and it REQUIRES already knowing the
//     construct's line range — which is precisely what an index provides. So
//     aligned containment is the ceiling for "a line cap that already has
//     mache", not for a bare line cap.
//   - UNALIGNED coverage is what a content-blind cap actually gets: the window
//     covers window/file_lines of the file, so reaching an arbitrary construct
//     costs about file_lines/window reads on average.
//
// The two are the same coin. A window covering half the median file halves the
// bytes per read AND doubles the reads needed to cover the file, so on
// tokens-to-answer the cap's byte advantage largely cancels. The bench can see
// the bytes; only an agent loop can see the reasoning cost of choosing offsets,
// which is why this is reported as containment rather than folded into a ratio.

// Containment reports, for one window size, how often a window can return a
// whole construct and how many reads a construct costs.
type Containment struct {
	WindowLines int `json:"window_lines"`
	Constructs  int `json:"constructs"`
	// Contained counts constructs that fit entirely in one ALIGNED window.
	Contained int `json:"contained"`
	// ContainedPct is Contained/Constructs — the best case, index-assisted.
	ContainedPct float64 `json:"contained_pct"`
	// Overflows is the count needing more than one window even when aligned.
	Overflows int `json:"overflows"`
	// AlignedReads is the mean windows per construct when perfectly aligned.
	AlignedReads float64 `json:"aligned_reads_per_construct"`
	// UnalignedReads is the mean windows to reach an arbitrary construct with
	// a content-blind cap: median_file_lines/window, floored at 1.
	UnalignedReads float64 `json:"unaligned_reads_per_construct"`
	// P50Lines / P95Lines describe the construct-size distribution the window
	// is being asked to contain.
	P50Lines int `json:"p50_construct_lines"`
	P95Lines int `json:"p95_construct_lines"`
	MaxLines int `json:"max_construct_lines"`
}

// computeContainment evaluates one window size against measured construct line
// spans. medianFileLines drives the unaligned estimate; pass 0 to skip it.
func computeContainment(window int, constructLines []int, medianFileLines int) Containment {
	c := Containment{WindowLines: window, Constructs: len(constructLines)}
	if window <= 0 || len(constructLines) == 0 {
		return c
	}
	sorted := append([]int(nil), constructLines...)
	sort.Ints(sorted)
	c.P50Lines = sorted[len(sorted)/2]
	c.P95Lines = sorted[(len(sorted)*95)/100]
	c.MaxLines = sorted[len(sorted)-1]

	totalReads := 0
	for _, n := range constructLines {
		if n <= window {
			c.Contained++
			totalReads++
			continue
		}
		c.Overflows++
		totalReads += (n + window - 1) / window // ceil
	}
	c.ContainedPct = float64(c.Contained) / float64(len(constructLines)) * 100
	c.AlignedReads = float64(totalReads) / float64(len(constructLines))

	c.UnalignedReads = 1
	if medianFileLines > window {
		c.UnalignedReads = float64(medianFileLines) / float64(window)
	}
	return c
}

// sweepContainment evaluates a range of window sizes so the point where a cap
// stops returning whole constructs is visible rather than assumed. 80 is
// pgr's baseline default and is always included by the caller.
func sweepContainment(windows, constructLines []int, medianFileLines int) []Containment {
	out := make([]Containment, 0, len(windows))
	for _, w := range windows {
		out = append(out, computeContainment(w, constructLines, medianFileLines))
	}
	return out
}
