package cmd

import (
	"context"
	"fmt"
	"testing"

	"github.com/agentic-research/mache/internal/graph"
)

// buildCommunityBenchGraph seeds a MemoryStore with `nClusters`
// distinct clusters of `clusterSize` nodes each. Within a cluster,
// every node references every shared token of that cluster — high
// intra-cluster modularity. Between clusters, no shared tokens —
// modularity score should be near 1.0 if Louvain works correctly.
//
// Total node count: nClusters * clusterSize. Total ref edges:
// nClusters * clusterSize * (clusterSize-1) / 2 (roughly), since
// every member references every other member.
func buildCommunityBenchGraph(b *testing.B, nClusters, clusterSize int) *graph.MemoryStore {
	b.Helper()
	store := graph.NewMemoryStore()
	for c := range nClusters {
		token := fmt.Sprintf("Cluster%dShared", c)
		for i := range clusterSize {
			nodeID := fmt.Sprintf("pkg/c%d/n%d", c, i)
			if err := store.AddRef(token, nodeID); err != nil {
				b.Fatal(err)
			}
		}
	}
	return store
}

// BenchmarkGetCommunities_Detect covers the full handler path:
// type-assert refsMapProvider, call RefsMap (snapshot copy), run
// Louvain, marshal JSON. Three scales bracket realistic codebases.
//
// 5×10  = 50 nodes, 5 clusters       — tiny project
// 10×50 = 500 nodes, 10 clusters     — medium codebase
// 20×100 = 2000 nodes, 20 clusters   — large monorepo slice
func BenchmarkGetCommunities_Detect(b *testing.B) {
	for _, tc := range []struct {
		name              string
		clusters, perSize int
	}{
		{"5x10", 5, 10},
		{"10x50", 10, 50},
		{"20x100", 20, 100},
	} {
		b.Run(tc.name, func(b *testing.B) {
			store := buildCommunityBenchGraph(b, tc.clusters, tc.perSize)
			handler := makeGetCommunitiesHandler(store)
			req := makeRequest(map[string]any{"min_size": float64(2)})
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

// BenchmarkGetCommunities_NoRefs measures the empty-graph cold path.
// Should be cheap; the handler short-circuits before Louvain runs.
// Guards against a regression that runs the algorithm on an empty
// refs map.
func BenchmarkGetCommunities_NoRefs(b *testing.B) {
	store := graph.NewMemoryStore()
	handler := makeGetCommunitiesHandler(store)
	req := makeRequest(map[string]any{"min_size": float64(2)})
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		_, err := handler(context.Background(), req)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGetCommunities_Summary covers the summary=true path —
// the response includes only top-level metrics, not the full
// community membership listing. Should be cheaper than the full
// detail path on the marshal side; Louvain runs the same.
func BenchmarkGetCommunities_Summary(b *testing.B) {
	store := buildCommunityBenchGraph(b, 10, 50)
	handler := makeGetCommunitiesHandler(store)
	req := makeRequest(map[string]any{"min_size": float64(2), "summary": true})
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		_, err := handler(context.Background(), req)
		if err != nil {
			b.Fatal(err)
		}
	}
}
