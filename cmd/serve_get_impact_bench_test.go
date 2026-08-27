package cmd

import (
	"context"
	"fmt"
	"io/fs"
	"testing"

	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/testutil"
)

// buildImpactBenchGraph seeds a chain graph: Root → L1.0…L1.fan → L2.0…L2.fan² → …
// At each level, every node from the previous level has `fanIn`
// distinct callers. Total nodes = 1 + fan + fan² + … fan^depth.
//
// Lets the benchmark drive get_impact with predictable blast radii:
// depth=1 returns fan, depth=2 returns fan + fan², etc.
func buildImpactBenchGraph(b *testing.B, fanIn, depth int) *graph.MemoryStore {
	b.Helper()
	store := graph.NewMemoryStore()
	store.AddRoot(&graph.Node{ID: "functions", Mode: fs.ModeDir})

	// Define the root symbol.
	root := "Root"
	store.AddNode(&graph.Node{
		ID:       "functions/Root",
		Mode:     fs.ModeDir,
		Children: []string{"functions/Root/source"},
	})
	store.AddNode(&graph.Node{ID: "functions/Root/source", Mode: 0})
	if err := store.AddDef(root, "functions/Root"); err != nil {
		b.Fatal(err)
	}

	// Level 1 callers: each calls Root.
	prev := []string{root}
	for d := 1; d <= depth; d++ {
		var cur []string
		for _, parent := range prev {
			for j := range fanIn {
				caller := fmt.Sprintf("L%d_p%s_c%d", d, parent, j)
				callerNodeID := fmt.Sprintf("functions/%s/source", caller)
				store.AddNode(&graph.Node{
					ID:       fmt.Sprintf("functions/%s", caller),
					Mode:     fs.ModeDir,
					Children: []string{callerNodeID},
				})
				store.AddNode(&graph.Node{ID: callerNodeID, Mode: 0})
				if err := store.AddDef(caller, fmt.Sprintf("functions/%s", caller)); err != nil {
					b.Fatal(err)
				}
				if err := store.AddRef(parent, callerNodeID); err != nil {
					b.Fatal(err)
				}
				cur = append(cur, caller)
			}
		}
		prev = cur
	}
	return store
}

// BenchmarkGetImpact_Callers covers the upstream-traversal path: who
// would break if I change Root? Three depths exercise the BFS at
// realistic blast radii.
//
// fan=4 depth=1 → 4 callers
// fan=4 depth=2 → 4 + 16 = 20 callers
// fan=4 depth=3 → 4 + 16 + 64 = 84 callers
// fan=4 depth=5 → 4 + 16 + 64 + 256 + 1024 = 1364 callers
func BenchmarkGetImpact_Callers(b *testing.B) {
	for _, depth := range []int{1, 2, 3, 5} {
		store := buildImpactBenchGraph(b, 4, depth)
		handler := makeGetImpactHandler(store)
		req := testutil.MakeRequest(map[string]any{
			"symbol":    "Root",
			"depth":     float64(depth),
			"direction": "callers",
		})
		b.Run(fmt.Sprintf("depth=%d", depth), func(b *testing.B) {
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

// BenchmarkGetImpact_NoDef covers the cold path — the requested
// symbol isn't defined anywhere. Should be cheap; the handler
// short-circuits before BFS runs.
func BenchmarkGetImpact_NoDef(b *testing.B) {
	store := buildImpactBenchGraph(b, 4, 2)
	handler := makeGetImpactHandler(store)
	req := testutil.MakeRequest(map[string]any{"symbol": "NotDefinedAnywhere"})
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		_, err := handler(context.Background(), req)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGetImpact_FanWidth varies fan-in at fixed depth=2 so the
// bench shows how cost scales with breadth (independent of depth).
// fan=2: 6 callers
// fan=4: 20
// fan=8: 72
func BenchmarkGetImpact_FanWidth(b *testing.B) {
	for _, fan := range []int{2, 4, 8} {
		store := buildImpactBenchGraph(b, fan, 2)
		handler := makeGetImpactHandler(store)
		req := testutil.MakeRequest(map[string]any{
			"symbol":    "Root",
			"depth":     float64(2),
			"direction": "callers",
		})
		b.Run(fmt.Sprintf("fan=%d", fan), func(b *testing.B) {
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
