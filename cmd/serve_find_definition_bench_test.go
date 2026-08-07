package cmd

import (
	"context"
	"fmt"
	"testing"

	"github.com/agentic-research/mache/graph"
)

// buildFindDefBenchGraph seeds a MemoryStore with N defs spanning a
// mix of "Func0001..FuncNNNN". Lets the benchmark hit the three
// resolution paths on a known distribution:
//
//   - anchored exact (case-sensitive): "Func0042" → 1 hit
//   - anchored case-insensitive: "func0042" → same 1 hit, slower
//   - fuzzy substring: "0042" with fuzzy=true → matches everything
//     containing "0042"
func buildFindDefBenchGraph(b *testing.B, nDefs int) *graph.MemoryStore {
	b.Helper()
	store := graph.NewMemoryStore()
	for i := range nDefs {
		token := fmt.Sprintf("Func%04d", i)
		dirID := fmt.Sprintf("functions/%s", token)
		if err := store.AddDef(token, dirID); err != nil {
			b.Fatal(err)
		}
	}
	return store
}

// BenchmarkFindDefinition_AnchoredExact covers the fast path —
// case-sensitive map lookup. O(1) regardless of map size; this
// benchmark guards against an accidental scan regression.
func BenchmarkFindDefinition_AnchoredExact(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		store := buildFindDefBenchGraph(b, n)
		handler := makeFindDefinitionHandler(store)
		req := makeRequest(map[string]any{"symbol": fmt.Sprintf("Func%04d", n/2)})
		b.Run(fmt.Sprintf("defs=%d", n), func(b *testing.B) {
			b.ResetTimer()
			b.ReportAllocs()
			for range b.N {
				_, err := handler(context.Background(), req)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkFindDefinition_CaseInsensitive measures the fallback
// triggered when anchored exact misses. Linear scan over defs map
// with strings.ToLower per token.
func BenchmarkFindDefinition_CaseInsensitive(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		store := buildFindDefBenchGraph(b, n)
		handler := makeFindDefinitionHandler(store)
		// Lowercase to bypass the case-sensitive branch.
		req := makeRequest(map[string]any{"symbol": fmt.Sprintf("func%04d", n/2)})
		b.Run(fmt.Sprintf("defs=%d", n), func(b *testing.B) {
			b.ResetTimer()
			b.ReportAllocs()
			for range b.N {
				_, err := handler(context.Background(), req)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkFindDefinition_FuzzySubstring covers the opt-in fuzzy
// fallback. Worst case: the anchored paths miss and the fuzzy scan
// runs over the whole defs map with strings.Contains in both
// directions.
func BenchmarkFindDefinition_FuzzySubstring(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		store := buildFindDefBenchGraph(b, n)
		handler := makeFindDefinitionHandler(store)
		// Substring that won't anchor-match (common substring shape).
		req := makeRequest(map[string]any{
			"symbol": "Func00",
			"fuzzy":  true,
		})
		b.Run(fmt.Sprintf("defs=%d", n), func(b *testing.B) {
			b.ResetTimer()
			b.ReportAllocs()
			for range b.N {
				_, err := handler(context.Background(), req)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkFindDefinition_NotFound measures the cold path — symbol
// doesn't exist anywhere. Without fuzzy=true it falls through both
// anchored paths and returns a not-found result. Cheap; this
// benchmark is a regression guard.
func BenchmarkFindDefinition_NotFound(b *testing.B) {
	store := buildFindDefBenchGraph(b, 1000)
	handler := makeFindDefinitionHandler(store)
	req := makeRequest(map[string]any{"symbol": "DefinitelyDoesNotExist"})
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		_, err := handler(context.Background(), req)
		if err != nil {
			b.Fatal(err)
		}
	}
}
