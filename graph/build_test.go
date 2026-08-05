package graph_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/graph"
	"github.com/stretchr/testify/require"
)

// TestBuildThenOpen is the end-to-end regression test for the gap this
// session closed in two steps: Open (previous commit) fixed querying an
// already-built .db, and Build closes the other half — producing one
// without shelling out to the mache CLI. A consumer using only this package
// can now go from a source tree to a queryable graph.
func TestBuildThenOpen(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	require.NoError(t, os.MkdirAll(src, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "main.go"), []byte(
		"package main\n\nfunc greet() string { return \"hi\" }\n\nfunc main() { greet() }\n",
	), 0o644))

	dbPath := filepath.Join(dir, "out.db")
	require.NoError(t, graph.Build(src, dbPath))

	info, err := os.Stat(dbPath)
	require.NoError(t, err)
	require.Greater(t, info.Size(), int64(0), "Build must produce a non-empty .db")

	g, err := graph.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })

	ids := g.LookupDef("greet")
	require.NotEmpty(t, ids, "LookupDef must find the function Build's own leyline parse just wrote")

	rows, err := g.QueryRefs("SELECT node_id FROM node_refs WHERE token = ?", "greet")
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	require.True(t, rows.Next(), "QueryRefs must find the call site main() makes to greet()")
}

// TestBuild_MissingSourceErrors proves Build surfaces a real error rather
// than silently producing an empty or partial .db.
func TestBuild_MissingSourceErrors(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "out.db")
	err := graph.Build(filepath.Join(t.TempDir(), "does-not-exist"), dbPath)
	require.Error(t, err)
}
