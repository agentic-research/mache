package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"
	"testing"

	"github.com/agentic-research/mache/internal/graph"
)

// buildReadFileBenchGraph seeds a MemoryStore with files of varying
// sizes addressed by stable IDs. Lets the benchmark exercise the
// single-read path at small/medium/large content scales without
// fighting through schema inference or template rendering.
func buildReadFileBenchGraph(b *testing.B) *graph.MemoryStore {
	b.Helper()
	store := graph.NewMemoryStore()
	store.AddRoot(&graph.Node{ID: "pkg", Mode: fs.ModeDir})

	// Small (~100 B), medium (~10 KB), large (~1 MB). Spans the
	// realistic content spectrum for source files / generated docs.
	for _, sz := range []struct {
		id   string
		size int
	}{
		{"pkg/small/source", 100},
		{"pkg/medium/source", 10_000},
		{"pkg/large/source", 1_000_000},
	} {
		store.AddNode(&graph.Node{
			ID:   sz.id,
			Mode: 0,
			Data: []byte(strings.Repeat("x", sz.size)),
		})
	}
	return store
}

// BenchmarkReadFile_Single covers the most common path: agent reads
// one file. Three scales (~100 B / ~10 KB / ~1 MB) capture the cost
// of GetNode + content-marshal across the realistic content spectrum.
func BenchmarkReadFile_Single(b *testing.B) {
	store := buildReadFileBenchGraph(b)
	handler := makeReadFileHandler(store)
	for _, name := range []string{"small", "medium", "large"} {
		b.Run(name, func(b *testing.B) {
			req := makeRequest(map[string]any{"path": "pkg/" + name + "/source"})
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

// BenchmarkReadFile_MissingPath measures the cold-error path. Hit
// when an agent asks for a path that doesn't exist (typo, race
// against a delete). Should be cheap; this benchmark guards against
// an accidental scan or expensive error-formatting regression.
func BenchmarkReadFile_MissingPath(b *testing.B) {
	store := buildReadFileBenchGraph(b)
	handler := makeReadFileHandler(store)
	req := makeRequest(map[string]any{"path": "pkg/does/not/exist"})
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		_, err := handler(context.Background(), req)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkReadFile_Batch covers the batched path — agents often
// fetch a fan-out of related files (e.g. all callers of a token).
// Tests 1, 10, and 50 small-file batches against the per-file size
// + total-size caps in makeReadFileHandler.
func BenchmarkReadFile_Batch(b *testing.B) {
	store := graph.NewMemoryStore()
	store.AddRoot(&graph.Node{ID: "pkg", Mode: fs.ModeDir})
	// Build 100 small files; benchmarks pick subsets via paths slice.
	const total = 100
	allPaths := make([]string, total)
	for i := range total {
		id := fmt.Sprintf("pkg/file_%03d/source", i)
		store.AddNode(&graph.Node{
			ID:   id,
			Mode: 0,
			Data: []byte(strings.Repeat("y", 200)),
		})
		allPaths[i] = id
	}
	handler := makeReadFileHandler(store)
	for _, n := range []int{1, 10, 50} {
		b.Run(fmt.Sprintf("batch=%d", n), func(b *testing.B) {
			payload, err := json.Marshal(allPaths[:n])
			if err != nil {
				b.Fatal(err)
			}
			req := makeRequest(map[string]any{"paths": string(payload)})
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
