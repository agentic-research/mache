package ingest

import (
	"fmt"
	"testing"

	"github.com/agentic-research/mache/internal/fixturedb"
)

// seedManyCalls builds the ley-line parse of a Go file whose one function
// body holds `nCalls` calls, half qualified (fmt.Println-style) and half
// bare (Helper-style), keyed as main.go. Returns the fixture; its DB() is
// the handle to pass to the walker and its DBPath() the file to re-open
// through another driver.
//
// Takes testing.TB so the growth-class GATE (ast_walker_growth_test.go) can
// seed the same shape the benchmark measures — one fixture, so the gate
// cannot drift from what is benchmarked (mache-c0537f).
func seedManyCalls(tb testing.TB, nCalls int) *fixturedb.FixtureDB {
	tb.Helper()
	b := fixturedb.New(tb, fixturedb.Leyline)
	b.Source("main.go", "go", "")
	end := 100*nCalls + 20
	b.ASTNode("main.go", "source_file", "main.go", fixturedb.Bytes(0, end))
	fn := "main.go/function_declaration"
	b.ASTNode(fn, "function_declaration", "main.go", fixturedb.Bytes(0, end))
	b.ASTNode(fn+"/identifier", "identifier", "main.go", fixturedb.Bytes(5, 6),
		fixturedb.Detail{Token: "A", Field: "name"})
	b.ASTNode(fn+"/parameter_list", "parameter_list", "main.go", fixturedb.Bytes(6, 8),
		fixturedb.Detail{Field: "parameters"})
	b.ASTNode(fn+"/block", "block", "main.go", fixturedb.Bytes(9, end), fixturedb.Detail{Field: "body"})
	for i := range nCalls {
		at := 100 * (i + 1)
		st := fmt.Sprintf("%s/block/expression_statement_%d", fn, i)
		call := st + "/call_expression"
		b.ASTNode(st, "expression_statement", "main.go", fixturedb.Bytes(at, at+20))
		b.ASTNode(call, "call_expression", "main.go", fixturedb.Bytes(at, at+20))
		if i%2 == 0 {
			sel := call + "/selector_expression"
			b.ASTNode(sel, "selector_expression", "main.go", fixturedb.Bytes(at, at+12),
				fixturedb.Detail{Field: "function"})
			b.ASTNode(sel+"/identifier", "identifier", "main.go", fixturedb.Bytes(at, at+4),
				fixturedb.Detail{Token: fmt.Sprintf("pkg%d", i/2), Field: "operand"})
			b.ASTNode(sel+"/field_identifier", "field_identifier", "main.go", fixturedb.Bytes(at+5, at+12),
				fixturedb.Detail{Token: fmt.Sprintf("Func%d", i/2), Field: "field"})
		} else {
			b.ASTNode(call+"/identifier", "identifier", "main.go", fixturedb.Bytes(at, at+12),
				fixturedb.Detail{Token: fmt.Sprintf("Bare%d", i/2), Field: "function"})
		}
		b.ASTNode(call+"/argument_list", "argument_list", "main.go", fixturedb.Bytes(at+12, at+14),
			fixturedb.Detail{Field: "arguments"})
	}
	_, f := b.Build()
	return f
}

// BenchmarkASTWalker_Extract sweeps call counts through each extractor;
// the per-call cost must stay flat as N grows (docs/reference/projection-performance.md).
// Each extractor is benchmarked through the same body so the two numbers are
// comparable: the only variable is the extractor.
func BenchmarkASTWalker_Extract(b *testing.B) {
	extractors := []struct {
		name string
		// extract runs one extraction and reports how many calls it found.
		extract func(w *ASTWalker) (int, error)
	}{
		{"Calls", func(w *ASTWalker) (int, error) {
			calls, err := w.ExtractCalls("main.go", "go")
			return len(calls), err
		}},
		{"QualifiedCalls", func(w *ASTWalker) (int, error) {
			calls, err := w.ExtractQualifiedCalls("main.go", "go")
			return len(calls), err
		}},
	}
	for _, ex := range extractors {
		for _, n := range []int{10, 100, 500} {
			b.Run(fmt.Sprintf("%s/calls=%d", ex.name, n), func(b *testing.B) {
				w := NewASTWalker(seedManyCalls(b, n).DB())
				// Warm-up to amortize one-time SQL planning.
				if _, err := ex.extract(w); err != nil {
					b.Fatal(err)
				}

				b.ResetTimer()
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					found, err := ex.extract(w)
					if err != nil {
						b.Fatal(err)
					}
					if found == 0 {
						b.Fatal("no calls extracted")
					}
				}
			})
		}
	}
}
