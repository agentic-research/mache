// Package projtest projects a topology over a small sample the way each
// documented command does, and walks the result the way a reader of the mount
// would. It is the shared machinery behind examples/examples_test.go and
// docs/reference/schema_test.go, so a doc snippet and an example schema are
// held to the same standard by the same code.
package projtest

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/agentic-research/mache/api"
	"github.com/agentic-research/mache/graph"
	"github.com/agentic-research/mache/internal/ingest"
	"github.com/agentic-research/mache/internal/lltest"
	"github.com/agentic-research/mache/internal/mcpharness"
	machetmpl "github.com/agentic-research/mache/internal/template"
	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"
)

// JSONDocument projects a JSON document through the engine, as `mache serve
// --schema S data.json` does.
func JSONDocument(t *testing.T, topo *api.Topology, path string) graph.Graph {
	t.Helper()
	store := graph.NewMemoryStore()
	abs, err := filepath.Abs(path)
	require.NoError(t, err)
	require.NoError(t, ingest.NewEngine(topo, store).Ingest(abs))
	return store
}

// VenturiResults writes records, a JSON array of Venturi records, into a
// `(id, record)` table named table — `results` when empty, which is the table
// Venturi and tools/mcp-fetch produce — and returns the db path. id is the
// record's `identifier` field.
func VenturiResults(t *testing.T, records []byte, table string) string {
	t.Helper()
	if table == "" {
		table = "results"
	}
	var recs []map[string]any
	require.NoError(t, json.Unmarshal(records, &recs))
	require.NotEmpty(t, recs)

	dbPath := filepath.Join(t.TempDir(), "data.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	_, err = db.Exec(`CREATE TABLE ` + table + ` (id TEXT PRIMARY KEY, record TEXT NOT NULL)`)
	require.NoError(t, err)
	for _, rec := range recs {
		body, err := json.Marshal(rec)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO `+table+` (id, record) VALUES (?, ?)`, rec["identifier"], string(body))
		require.NoError(t, err)
	}
	return dbPath
}

// VenturiDirect serves a results db over the direct SQL backend, as `mache
// serve --schema S data.db` and the mount do.
func VenturiDirect(t *testing.T, topo *api.Topology, dbPath string) graph.Graph {
	t.Helper()
	sg, err := graph.OpenSQLiteGraph(dbPath, topo, machetmpl.Render)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sg.Close() })
	return sg
}

// VenturiEngine streams a results db through the engine, the path
// `mache --data data.db --out` takes.
func VenturiEngine(t *testing.T, topo *api.Topology, dbPath string) graph.Graph {
	t.Helper()
	store := graph.NewMemoryStore()
	require.NoError(t, ingest.NewEngine(topo, store).Ingest(dbPath))
	return store
}

// LeylineSource parses src (a file or a directory) with the pinned leyline
// and projects it, as `mache build --schema S` does. MACHE_HOME is isolated
// for the rest of the test.
func LeylineSource(t *testing.T, topo *api.Topology, src string) graph.Graph {
	t.Helper()
	mcpharness.IsolatedHome(t)
	store := graph.NewMemoryStore()
	lltest.IngestSourceViaLeyline(t, ingest.NewEngine(topo, store), src)
	return store
}

// Walk visits every node under the root, as a reader of the mount would, and
// returns the directory and leaf paths it saw and what it found wrong: a node
// named from a field the record does not have (`<no value>`), a leaf whose
// content contains one, and a leaf that renders empty. Leaves whose base name
// is in nullable may render empty.
func Walk(t *testing.T, g graph.Graph, nullable []string) (dirs, leaves, problems []string) {
	t.Helper()
	var walk func(id string)
	walk = func(id string) {
		children, err := g.ListChildren(id)
		require.NoError(t, err, "list %q", id)
		for _, child := range children {
			name := filepath.Base(child)
			if strings.Contains(name, "<no value>") {
				problems = append(problems, child+": named from a missing field")
			}
			node, err := g.GetNode(child)
			require.NoError(t, err, "get %q", child)
			if node.Mode.IsDir() {
				dirs = append(dirs, child)
				walk(child)
				continue
			}
			leaves = append(leaves, child)
			content := Read(t, g, node)
			switch {
			case strings.Contains(content, "<no value>"):
				problems = append(problems, child+": content rendered from a missing field")
			case strings.TrimSpace(content) == "" && !slices.Contains(nullable, name):
				problems = append(problems, child+": renders empty")
			}
		}
	}
	walk("")
	sort.Strings(dirs)
	sort.Strings(leaves)
	return dirs, leaves, problems
}

// Read returns a leaf's whole content.
func Read(t *testing.T, g graph.Graph, node *graph.Node) string {
	t.Helper()
	buf := make([]byte, node.ContentSize())
	n, err := g.ReadContent(node.ID, buf, 0)
	require.NoError(t, err, "read %q", node.ID)
	return string(buf[:n])
}

// WriteFiles writes files (name → content) into a fresh temp dir and returns it.
func WriteFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
	}
	return dir
}
