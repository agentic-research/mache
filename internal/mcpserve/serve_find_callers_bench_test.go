package mcpserve

import (
	"context"
	"fmt"
	"io/fs"
	"testing"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/testutil"
)

// buildFindCallersBenchGraph seeds a MemoryStore where one popular
// token (`hot`) has nFanout distinct callers and a single rare token
// (`cold`) has one caller. Lets the benchmark measure both common and
// long-tail lookups.
func buildFindCallersBenchGraph(b *testing.B, nFanout int) *graph.MemoryStore {
	b.Helper()
	store := graph.NewMemoryStore()
	store.AddRoot(&graph.Node{ID: "pkg", Mode: fs.ModeDir})

	for i := range nFanout {
		callerID := fmt.Sprintf("pkg/Caller%05d/source", i)
		// AddRef expects (token, nodeID).
		if err := store.AddRef("hot", callerID); err != nil {
			b.Fatal(err)
		}
		store.AddNode(&graph.Node{ID: callerID})
	}
	if err := store.AddRef("cold", "pkg/Other/source"); err != nil {
		b.Fatal(err)
	}
	store.AddNode(&graph.Node{ID: "pkg/Other/source"})
	return store
}

// BenchmarkFindCallers_HotToken measures the agent's most common path:
// look up a token that's referenced from many callers. Covers the
// graph.GetCallers + JSON-marshal pipeline. The LSP-supplement branch
// is exercised lazily — MemoryStore implements graph.RefsQuerier but its
// node_refs table doesn't contain the token, so the LSP query returns
// empty and the handler falls through to the no-LSP shape.
func BenchmarkFindCallers_HotToken(b *testing.B) {
	for _, n := range []int{1, 50, 500, 5000} {
		b.Run(fmt.Sprintf("fanout=%d", n), func(b *testing.B) {
			store := buildFindCallersBenchGraph(b, n)
			handler := makeFindCallersHandler(store)
			req := testutil.MakeRequest(map[string]any{"token": "hot"})
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

// BenchmarkFindCallers_RareToken measures the long-tail case (single
// caller). Useful as a baseline floor — agents browsing find_callers
// across many symbols hit this shape constantly.
func BenchmarkFindCallers_RareToken(b *testing.B) {
	store := buildFindCallersBenchGraph(b, 100)
	handler := makeFindCallersHandler(store)
	req := testutil.MakeRequest(map[string]any{"token": "cold"})
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		_, err := handler(context.Background(), req)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkFindCallers_MissingToken measures the empty-result path —
// hit when an agent asks about a token that isn't referenced anywhere.
// Should be cheap; this benchmark guards against an accidental
// regression that scans the full refs map.
func BenchmarkFindCallers_MissingToken(b *testing.B) {
	store := buildFindCallersBenchGraph(b, 1000)
	handler := makeFindCallersHandler(store)
	req := testutil.MakeRequest(map[string]any{"token": "neverReferenced"})
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		_, err := handler(context.Background(), req)
		if err != nil {
			b.Fatal(err)
		}
	}
}
