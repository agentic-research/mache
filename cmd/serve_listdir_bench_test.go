package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"testing"

	"github.com/agentic-research/mache/graph"
)

// buildBenchGraph constructs a directory with `nChildren` subdirs, each
// holding a single source file. Mirrors the shape mache projects for a
// large package or a monorepo top-level layout (where the bottleneck
// reported in mache-og26 was observed).
func buildBenchGraph(b *testing.B, nChildren int) *graph.MemoryStore {
	b.Helper()
	store := graph.NewMemoryStore()

	rootID := "pkg"
	root := &graph.Node{ID: rootID, Mode: fs.ModeDir}
	root.Children = make([]string, nChildren)
	for i := range nChildren {
		root.Children[i] = fmt.Sprintf("%s/child_%04d", rootID, i)
	}
	store.AddRoot(root)

	for i := range nChildren {
		dirID := fmt.Sprintf("%s/child_%04d", rootID, i)
		store.AddNode(&graph.Node{
			ID:       dirID,
			Mode:     fs.ModeDir,
			Children: []string{dirID + "/source"},
		})
		store.AddNode(&graph.Node{
			ID:   dirID + "/source",
			Mode: 0,
			Data: fmt.Appendf(nil, "func F%04d() {}", i),
		})
	}
	return store
}

// BenchmarkListDirectory_LegacyShape mirrors the prior handler
// (ListChildren + N×GetNode + entry build + JSON marshal). Apples-to-
// apples comparison with BenchmarkListDirectory below.
//
// On MemoryStore the two shapes are within ~10% of each other — the
// real bead-mache-og26 win is the container-name skip in the new
// handler, which avoids per-readdir GetCallers/GetCallees probes
// (those are SQL queries on SQLiteGraph and tree-sitter parses on
// MemoryStore source files).
func BenchmarkListDirectory_LegacyShape(b *testing.B) {
	for _, n := range []int{50, 500, 5000} {
		b.Run(fmt.Sprintf("children=%d", n), func(b *testing.B) {
			store := buildBenchGraph(b, n)
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if err := legacyListDirShape(store, "pkg"); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// legacyListDirShape replays the pre-mache-og26 handler shape — same
// final entries[]+JSON output as the new handler, but using the N+1
// GetNode pattern instead of ListChildStats. Used by the baseline
// benchmark above.
func legacyListDirShape(g graph.Graph, path string) error {
	children, err := g.ListChildren(path)
	if err != nil {
		return err
	}
	entries := make([]nodeEntry, 0, len(children))
	for _, childID := range children {
		node, err := g.GetNode(childID)
		if err != nil {
			continue
		}
		typ := "file"
		if node.Mode.IsDir() {
			typ = "dir"
		}
		entries = append(entries, nodeEntry{
			Name: filenameOf(childID),
			Path: childID,
			Type: typ,
			Size: node.ContentSize(),
		})
	}
	_, err = json.MarshalIndent(entries, "", "  ")
	return err
}

// filenameOf returns the basename of a slash-delimited graph node ID.
// Avoids dragging filepath.Base into a hot loop.
func filenameOf(id string) string {
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == '/' {
			return id[i+1:]
		}
	}
	return id
}

// BenchmarkListDirectory measures the cost of one list_directory call
// over a parent with `n` children. Bead mache-og26: prior to the
// ListChildStats refactor, this was an N+1-GetNode pattern that scaled
// poorly under lock contention.
func BenchmarkListDirectory(b *testing.B) {
	for _, n := range []int{50, 500, 5000} {
		b.Run(fmt.Sprintf("children=%d", n), func(b *testing.B) {
			store := buildBenchGraph(b, n)
			handler := makeListDirHandler(store)
			req := makeRequest(map[string]any{"path": "pkg"})
			ctx := context.Background()

			if _, err := handler(ctx, req); err != nil {
				b.Fatal(err)
			}

			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				result, err := handler(ctx, req)
				if err != nil {
					b.Fatal(err)
				}
				if result == nil || result.IsError {
					b.Fatal("list_directory returned an error result")
				}
			}
		})
	}
}
