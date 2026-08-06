package cmd

import (
	"context"
	"fmt"
	"testing"

	"github.com/agentic-research/mache/graph"
)

// buildSearchBenchGraph seeds a MemoryStore with N distinct defs.
// Tokens span "Func0001..FuncNNNN" so glob patterns hit a known
// distribution: 'Func1%' matches the first 10% (1000–1999 range
// when nDefs=10000), 'Func%' matches all, 'NoMatch%' matches none.
func buildSearchBenchGraph(b *testing.B, nDefs int) *graph.MemoryStore {
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

// BenchmarkSearch_DefinitionMode covers the in-memory defs-map path
// (role=definition). The handler scans the entire defs map with
// LIKE-style matching; runtime is dominated by map iteration plus
// pattern matching cost.
//
// Three pattern shapes per scale:
//   - 'Func1%' — matches ~10% of tokens (selective)
//   - 'Func%'  — matches everything (worst case before limit kicks in)
//   - 'NoMatch%' — matches nothing (cold path)
func BenchmarkSearch_DefinitionMode(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		store := buildSearchBenchGraph(b, n)
		handler := makeSearchHandler(store)
		for _, pat := range []string{"Func1%", "Func%", "NoMatch%"} {
			b.Run(fmt.Sprintf("defs=%d/%s", n, pat), func(b *testing.B) {
				req := makeRequest(map[string]any{
					"pattern": pat,
					"role":    "definition",
				})
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
}

// BenchmarkSearch_TypeFilter covers the typeFilter branch — adds an
// extra `strings.Contains` check per match. Compares with/without to
// gauge the overhead. The test seeds tokens whose IDs match the
// 'functions' container so 'type=functions' keeps everything.
func BenchmarkSearch_TypeFilter(b *testing.B) {
	store := buildSearchBenchGraph(b, 1000)
	handler := makeSearchHandler(store)

	b.Run("no_filter", func(b *testing.B) {
		req := makeRequest(map[string]any{"pattern": "Func1%", "role": "definition"})
		b.ResetTimer()
		b.ReportAllocs()
		for range b.N {
			if _, err := handler(context.Background(), req); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("functions_filter", func(b *testing.B) {
		req := makeRequest(map[string]any{
			"pattern": "Func1%",
			"role":    "definition",
			"type":    "functions",
		})
		b.ResetTimer()
		b.ReportAllocs()
		for range b.N {
			if _, err := handler(context.Background(), req); err != nil {
				b.Fatal(err)
			}
		}
	})
}
