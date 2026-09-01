package resolve

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/agentic-research/mache/build"
	"github.com/agentic-research/mache/graph"
)

// buildAndOpen leyline-parses dir into a temp .db (build.Parse) and opens it
// (graph.Open) — the "resolve this directory to a queryable graph" tail
// shared by every filesystem-directory-based resolver (GoModResolver once
// `go list` names a directory; LocalPathResolver once its locator is
// anchored to one). tmpPattern is passed straight to os.MkdirTemp so each
// caller's temp dirs stay visually distinguishable (e.g.
// "mache-gomod-resolve-*").
func buildAndOpen(tmpPattern, dir string) (graph.Graph, error) {
	dbDir, err := os.MkdirTemp("", tmpPattern)
	if err != nil {
		return nil, fmt.Errorf("resolve: create temp dir: %w", err)
	}
	dbPath := filepath.Join(dbDir, "resolved.db")

	if err := build.Parse(dir, dbPath); err != nil {
		_ = os.RemoveAll(dbDir)
		return nil, fmt.Errorf("resolve: build %s: %w", dir, err)
	}

	g, err := graph.Open(dbPath)
	if err != nil {
		_ = os.RemoveAll(dbDir)
		return nil, fmt.Errorf("resolve: open resolved db: %w", err)
	}
	return g, nil
}
