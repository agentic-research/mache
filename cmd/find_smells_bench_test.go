package cmd

import (
	"fmt"
	"testing"
)

// mache-238673 phase-1 bench — quantifies the node_hash-memo win on
// cyclomatic_complexity. The scenario models real source: many function
// occurrences, but a much smaller number of DISTINCT subtrees (boilerplate,
// generated code, copy-paste all dedup to one merkle node_hash). A warm memo
// turns the O(occurrences × subtree) branch-count join into an O(occurrences)
// scan + map lookup — the sub-linear re-run the merkle cash-out promises.
//
// Compare the three sub-benchmarks at a given size:
//   FullScan  — runSmellRule every iteration (today's behavior)
//   MemoCold  — fresh memo every iteration (first-run cost; parity with FullScan)
//   MemoWarm  — memo warmed once, reused every iteration (the incremental re-run)
// MemoWarm/ns-per-op well below FullScan is the phase-1 result.

// seedCyclomaticMemoFixture builds an _ast db of nFuncs function occurrences
// spread over `distinct` unique subtrees, each with a node_hash. Every subtree
// has three counted branches, so the metric is a constant 3 — the point is the
// scan cost, not the metric spread.
func seedCyclomaticMemoFixture(b *testing.B, nFuncs, distinct int) []fixFunc {
	b.Helper()
	shapes := make([][]string, distinct)
	hashes := make([][]byte, distinct)
	for d := range distinct {
		shapes[d] = []string{"if_statement", "for_statement", "case_clause", "block"}
		hashes[d] = hashFor(fmt.Sprintf("subtree-%d", d))
	}
	funcs := make([]fixFunc, 0, nFuncs)
	for i := range nFuncs {
		d := i % distinct
		funcs = append(funcs, fixFunc{
			sourceID:  fmt.Sprintf("pkg/%d/file.go", i/50),
			nodeID:    fmt.Sprintf("pkg/%d/file.go/F%06d", i/50, i),
			hash:      hashes[d],
			branches:  shapes[d],
			startByte: i * 1000,
			startRow:  i * 30,
		})
	}
	return funcs
}

func BenchmarkCyclomaticMemo(b *testing.B) {
	// 5% distinct: heavy duplication, the memo's best case.
	for _, n := range []int{1000, 10000} {
		distinct := n / 20
		funcs := seedCyclomaticMemoFixture(b, n, distinct)
		db := buildASTFixture(b, funcs)
		qg := &sqlDBQuerier{db: db}
		rule := cyclomaticRule(b)

		b.Run(fmt.Sprintf("FullScan/n=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				if _, err := runSmellRule(qg, rule, "", oracleLimit); err != nil {
					b.Fatal(err)
				}
			}
		})

		b.Run(fmt.Sprintf("MemoCold/n=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				memo := newSmellMemo()
				if _, err := runCyclomaticComplexityMemo(qg, "", oracleLimit, memo); err != nil {
					b.Fatal(err)
				}
			}
		})

		b.Run(fmt.Sprintf("MemoWarm/n=%d", n), func(b *testing.B) {
			memo := newSmellMemo()
			if _, err := runCyclomaticComplexityMemo(qg, "", oracleLimit, memo); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := runCyclomaticComplexityMemo(qg, "", oracleLimit, memo); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
