package build_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-research/mache/build"
	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/schema"
	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"
)

// TestBuildThenOpen is the end-to-end regression test for the gap this
// session closed in two steps: graph.Open fixed querying an already-built
// .db, and build.Parse closes the other half — producing one without
// shelling out to the mache CLI. A consumer using only this package
// can now go from a source tree to a queryable graph.
func TestBuildThenOpen(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	require.NoError(t, os.MkdirAll(src, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "main.go"), []byte(
		"package main\n\nfunc greet() string { return \"hi\" }\n\nfunc main() { greet() }\n",
	), 0o644))

	dbPath := filepath.Join(dir, "out.db")
	require.NoError(t, build.Parse(src, dbPath))

	info, err := os.Stat(dbPath)
	require.NoError(t, err)
	require.Greater(t, info.Size(), int64(0), "Parse must produce a non-empty .db")

	g, err := graph.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })

	ids := g.LookupDef("greet")
	require.NotEmpty(t, ids, "LookupDef must find the function Parse's own leyline run just wrote")

	rows, err := g.QueryRefs("SELECT node_id FROM node_refs WHERE token = ?", "greet")
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	require.True(t, rows.Next(), "QueryRefs must find the call site main() makes to greet()")
}

// TestBuild_MissingSourceErrors proves Build surfaces a real error rather
// than silently producing an empty or partial .db.
func TestBuild_MissingSourceErrors(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "out.db")
	err := build.Parse(filepath.Join(t.TempDir(), "does-not-exist"), dbPath)
	require.Error(t, err)
}

func TestParseWithSchemaProjectsCallerTopology(t *testing.T) {
	dir := t.TempDir()
	sourceDir := filepath.Join(dir, "src")
	require.NoError(t, os.MkdirAll(sourceDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sourceDir, "main.go"), []byte(`package sample

func Use() string { return "ok" }
`), 0o644))

	topology, err := schema.Parse([]byte(`{
  "version": "v1",
  "nodes": [{
    "name": "functions",
    "selector": "$",
    "language": "go",
    "children": [{
      "name": "{{.name}}",
      "selector": "(function_declaration name: (identifier) @name) @scope",
      "files": [{"name": "source", "content_template": "{{.scope}}"}]
    }]
  }]
}`))
	require.NoError(t, err)

	outputDB := filepath.Join(dir, "projected.db")
	require.NoError(t, build.ParseWithSchema(sourceDir, outputDB, topology))
	require.Equal(t, 1, sqliteCount(t, outputDB,
		`SELECT count(*) FROM nodes WHERE id = 'functions/Use/source'`))
}

func TestParseWithSchemaRefProjectsPreset(t *testing.T) {
	dir := t.TempDir()
	sourceDir := filepath.Join(dir, "src")
	require.NoError(t, os.MkdirAll(sourceDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sourceDir, "main.go"), []byte(`package sample

func Use() string { return "ok" }
`), 0o644))

	outputDB := filepath.Join(dir, "projected.db")
	require.NoError(t, build.ParseWithSchemaRef(sourceDir, outputDB, "go", sourceDir))
	require.Equal(t, 1, sqliteCount(t, outputDB,
		`SELECT count(*) FROM nodes WHERE id = 'sample/functions/Use/source'`))
}

func sqliteCount(t *testing.T, dbPath, query string, args ...any) int {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	var count int
	require.NoError(t, db.QueryRow(query, args...).Scan(&count))
	return count
}
