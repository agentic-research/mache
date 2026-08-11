package ingest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
)

// loadGoSchemaForBench parses examples/go-schema.json once. The
// schema captures functions, types, and refs — covers the realistic
// ingestion shape for Go source.
func loadGoSchemaForBench(b *testing.B) *api.Topology {
	b.Helper()
	raw, err := os.ReadFile("../../examples/go-schema.json")
	if err != nil {
		b.Fatal(err)
	}
	var schema api.Topology
	if err := json.Unmarshal(raw, &schema); err != nil {
		b.Fatal(err)
	}
	return &schema
}

// writeGoTreeForBench lays down a synthetic Go source tree of
// nFiles, each containing fnsPerFile small functions. The tree is
// flat under the temp dir (no nested packages) — ingestion cost
// is dominated by parser invocations + symbol writes.
func writeGoTreeForBench(b *testing.B, nFiles, fnsPerFile int) string {
	b.Helper()
	dir := b.TempDir()
	for f := range nFiles {
		var sb strings.Builder
		fmt.Fprintf(&sb, "package pkg%d\n\n", f)
		for i := range fnsPerFile {
			fmt.Fprintf(&sb, "func F%d_%d() { _ = 1 }\n", f, i)
		}
		path := filepath.Join(dir, fmt.Sprintf("file_%04d.go", f))
		if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
			b.Fatal(err)
		}
	}
	return dir
}

// BenchmarkIngest_SourceDir covers the in-process source-ingestion
// path used by `mache serve <dir>` when leyline auto-invoke is
// disabled. Tree-sitter parses each file, walks the AST per the Go
// schema, writes nodes/refs/defs into a fresh MemoryStore.
//
// Three scales:
//   - 10 files × 5 fns  (50 total)   — small project
//   - 100 × 10 (1K)                  — medium
//   - 500 × 20 (10K)                 — large
//
// Cost is roughly file-count × parser-pool-stall + total-symbol-count
// × store-write. Bench measures per-Ingest time so growth in either
// dimension shows up as throughput change.
func BenchmarkIngest_SourceDir(b *testing.B) {
	schema := loadGoSchemaForBench(b)

	for _, tc := range []struct {
		name           string
		files, perFile int
	}{
		{"small_50", 10, 5},
		{"medium_1k", 100, 10},
		{"large_10k", 500, 20},
	} {
		b.Run(tc.name, func(b *testing.B) {
			dir := writeGoTreeForBench(b, tc.files, tc.perFile)
			b.ResetTimer()
			b.ReportAllocs()
			for range b.N {
				// Fresh store per iteration so we're not measuring
				// accumulated state. Mirrors how `mache serve` builds
				// a graph from scratch on startup.
				store := graph.NewMemoryStore()
				engine := NewEngine(schema, store)
				if err := engine.Ingest(dir); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkIngest_SingleFile measures the per-file overhead. Useful
// floor — small projects don't pay the worker-pool spool-up cost,
// so this isolates the parse + walk + write per construct.
func BenchmarkIngest_SingleFile(b *testing.B) {
	schema := loadGoSchemaForBench(b)
	dir := writeGoTreeForBench(b, 1, 10) // 1 file, 10 functions

	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		store := graph.NewMemoryStore()
		engine := NewEngine(schema, store)
		if err := engine.Ingest(dir); err != nil {
			b.Fatal(err)
		}
	}
}
